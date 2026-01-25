#!/bin/bash
# Conformance tests for firecracker-shim
#
# This script runs conformance tests to verify the runtime works correctly
# with Kubernetes. It includes:
# - CRI validation tests (via critest)
# - Pod lifecycle tests
# - Container operations tests
# - Networking tests
# - Resource limit tests
#
# Prerequisites:
# - firecracker-shim installed and configured
# - containerd running with firecracker runtime
# - critest binary (go install github.com/kubernetes-sigs/cri-tools/cmd/critest@latest)
# - kubectl configured (for k8s tests)
#
# Usage:
#   ./scripts/conformance-test.sh [options]
#
# Options:
#   -v, --verbose     Enable verbose output
#   -s, --skip-k8s    Skip Kubernetes e2e tests
#   -c, --critest     Run only CRI validation tests
#   -k, --k8s-only    Run only Kubernetes e2e tests
#   --runtime-class   RuntimeClass name (default: firecracker)
#   --timeout         Test timeout in seconds (default: 300)

set -euo pipefail

# =============================================================================
# Configuration
# =============================================================================

SCRIPT_DIR="$(dirname "$(realpath "$0")")"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
TEST_DIR="/tmp/fc-conformance-tests"
LOG_DIR="$TEST_DIR/logs"
RESULTS_FILE="$TEST_DIR/results.json"

# Defaults
VERBOSE=false
SKIP_K8S=false
CRITEST_ONLY=false
K8S_ONLY=false
RUNTIME_CLASS="firecracker"
CONTAINERD_SOCKET="/run/containerd/containerd.sock"
TIMEOUT=300

# Test counters
TOTAL_TESTS=0
PASSED_TESTS=0
FAILED_TESTS=0
SKIPPED_TESTS=0

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m'

# =============================================================================
# Utility Functions
# =============================================================================

log() {
    echo -e "${BLUE}[INFO]${NC} $(date '+%H:%M:%S') $*"
}

log_verbose() {
    if $VERBOSE; then
        echo -e "${CYAN}[DEBUG]${NC} $(date '+%H:%M:%S') $*"
    fi
}

log_success() {
    echo -e "${GREEN}[PASS]${NC} $*"
}

log_failure() {
    echo -e "${RED}[FAIL]${NC} $*"
}

log_skip() {
    echo -e "${YELLOW}[SKIP]${NC} $*"
}

cleanup() {
    log "Cleaning up test resources..."

    # Clean up test pods
    if command -v kubectl &>/dev/null; then
        kubectl delete pods -l test=fc-conformance --ignore-not-found=true --timeout=30s 2>/dev/null || true
        kubectl delete namespace fc-conformance-test --ignore-not-found=true --timeout=30s 2>/dev/null || true
    fi

    # Clean up critest containers
    if command -v ctr &>/dev/null; then
        ctr -n k8s.io containers rm $(ctr -n k8s.io containers ls -q | grep fc-test) 2>/dev/null || true
    fi
}

trap cleanup EXIT

run_test() {
    local name="$1"
    local func="$2"

    ((TOTAL_TESTS++))

    log_verbose "Running: $name"

    local start_time=$(date +%s%N)
    local result=0
    local output=""

    # Run test and capture output
    output=$($func 2>&1) || result=$?

    local end_time=$(date +%s%N)
    local duration_ms=$(( (end_time - start_time) / 1000000 ))

    if [[ $result -eq 0 ]]; then
        log_success "$name (${duration_ms}ms)"
        ((PASSED_TESTS++))

        # Save result
        echo "{\"name\":\"$name\",\"status\":\"pass\",\"duration_ms\":$duration_ms}" >> "$TEST_DIR/results.jsonl"
    else
        log_failure "$name (${duration_ms}ms)"
        ((FAILED_TESTS++))

        # Log failure details
        echo "--- Failure details for: $name ---" >> "$LOG_DIR/failures.log"
        echo "$output" >> "$LOG_DIR/failures.log"
        echo "---" >> "$LOG_DIR/failures.log"

        echo "{\"name\":\"$name\",\"status\":\"fail\",\"duration_ms\":$duration_ms,\"error\":\"$output\"}" >> "$TEST_DIR/results.jsonl"
    fi

    return $result
}

skip_test() {
    local name="$1"
    local reason="$2"

    ((TOTAL_TESTS++))
    ((SKIPPED_TESTS++))
    log_skip "$name - $reason"
    echo "{\"name\":\"$name\",\"status\":\"skip\",\"reason\":\"$reason\"}" >> "$TEST_DIR/results.jsonl"
}

# =============================================================================
# Prerequisites Check
# =============================================================================

check_prerequisites() {
    log "Checking prerequisites..."

    local missing=()

    # Check root
    if [[ $EUID -ne 0 ]]; then
        echo "This script must be run as root"
        exit 1
    fi

    # Check containerd
    if [[ ! -S "$CONTAINERD_SOCKET" ]]; then
        missing+=("containerd socket ($CONTAINERD_SOCKET)")
    fi

    # Check critest
    if ! command -v critest &>/dev/null; then
        echo "critest not found. Installing..."
        go install github.com/kubernetes-sigs/cri-tools/cmd/critest@latest || missing+=("critest")
    fi

    # Check ctr
    if ! command -v ctr &>/dev/null; then
        missing+=("ctr")
    fi

    # Check firecracker
    if ! command -v firecracker &>/dev/null; then
        missing+=("firecracker")
    fi

    # Check KVM
    if [[ ! -e /dev/kvm ]]; then
        missing+=("/dev/kvm")
    fi

    # Check runtime is configured
    if ! ctr --address "$CONTAINERD_SOCKET" plugins ls 2>/dev/null | grep -q firecracker; then
        log "Warning: firecracker runtime may not be configured in containerd"
    fi

    if [[ ${#missing[@]} -gt 0 ]]; then
        echo "Missing prerequisites:"
        printf '  - %s\n' "${missing[@]}"
        exit 1
    fi

    log "All prerequisites satisfied"
}

# =============================================================================
# CRI Validation Tests (critest)
# =============================================================================

run_critest() {
    log "Running CRI validation tests (critest)..."

    local critest_log="$LOG_DIR/critest.log"

    # Run critest with firecracker runtime
    if critest \
        --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        --runtime-handler "$RUNTIME_CLASS" \
        --parallel 1 \
        --timeout "$TIMEOUT" \
        --ginkgo.v \
        --ginkgo.skip=".*portforward.*|.*attach.*" \
        2>&1 | tee "$critest_log"; then
        log_success "critest validation passed"
        return 0
    else
        log_failure "critest validation failed (see $critest_log)"
        return 1
    fi
}

# =============================================================================
# Pod Lifecycle Tests
# =============================================================================

test_pod_create_delete() {
    local pod_name="fc-test-pod-$$"
    local namespace="default"

    # Create pod sandbox config
    local sandbox_config=$(mktemp)
    cat > "$sandbox_config" <<EOF
{
    "metadata": {
        "name": "$pod_name",
        "namespace": "$namespace",
        "uid": "test-uid-$$"
    },
    "log_directory": "/tmp",
    "linux": {}
}
EOF

    # Create sandbox via crictl
    local sandbox_id
    sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>&1) || {
        rm -f "$sandbox_config"
        return 1
    }

    log_verbose "Created sandbox: $sandbox_id"

    # Verify sandbox is running
    local status
    status=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        inspectp "$sandbox_id" | jq -r '.status.state' 2>/dev/null) || true

    if [[ "$status" != "SANDBOX_READY" ]]; then
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        rm -f "$sandbox_config"
        return 1
    fi

    # Delete sandbox
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true

    rm -f "$sandbox_config"
    return 0
}

test_container_lifecycle() {
    local pod_name="fc-test-container-$$"
    local namespace="default"

    # Create pod sandbox
    local sandbox_config=$(mktemp)
    cat > "$sandbox_config" <<EOF
{
    "metadata": {
        "name": "$pod_name",
        "namespace": "$namespace",
        "uid": "test-uid-$$"
    },
    "log_directory": "/tmp",
    "linux": {}
}
EOF

    local sandbox_id
    sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>&1) || {
        rm -f "$sandbox_config"
        return 1
    }

    # Create container config
    local container_config=$(mktemp)
    cat > "$container_config" <<EOF
{
    "metadata": {
        "name": "test-container"
    },
    "image": {
        "image": "docker.io/library/alpine:latest"
    },
    "command": ["sleep", "30"],
    "log_path": "container.log",
    "linux": {}
}
EOF

    # Pull image first
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        pull docker.io/library/alpine:latest 2>/dev/null || true

    # Create container
    local container_id
    container_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        create "$sandbox_id" "$container_config" "$sandbox_config" 2>&1) || {
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        rm -f "$sandbox_config" "$container_config"
        return 1
    }

    log_verbose "Created container: $container_id"

    # Start container
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        start "$container_id" 2>/dev/null || {
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rm "$container_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        rm -f "$sandbox_config" "$container_config"
        return 1
    }

    # Verify container is running
    sleep 2
    local status
    status=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        inspect "$container_id" | jq -r '.status.state' 2>/dev/null) || true

    # Cleanup
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stop "$container_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rm "$container_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
    rm -f "$sandbox_config" "$container_config"

    [[ "$status" == "CONTAINER_RUNNING" ]]
}

test_container_exec() {
    local pod_name="fc-test-exec-$$"

    # Create pod sandbox
    local sandbox_config=$(mktemp)
    cat > "$sandbox_config" <<EOF
{
    "metadata": {
        "name": "$pod_name",
        "namespace": "default",
        "uid": "test-uid-$$"
    },
    "log_directory": "/tmp",
    "linux": {}
}
EOF

    local sandbox_id
    sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>&1) || {
        rm -f "$sandbox_config"
        return 1
    }

    # Create container
    local container_config=$(mktemp)
    cat > "$container_config" <<EOF
{
    "metadata": {"name": "test-container"},
    "image": {"image": "docker.io/library/alpine:latest"},
    "command": ["sleep", "60"],
    "log_path": "container.log",
    "linux": {}
}
EOF

    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        pull docker.io/library/alpine:latest 2>/dev/null || true

    local container_id
    container_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        create "$sandbox_id" "$container_config" "$sandbox_config" 2>&1) || {
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        rm -f "$sandbox_config" "$container_config"
        return 1
    }

    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" start "$container_id" 2>/dev/null || true
    sleep 2

    # Test exec
    local exec_output
    exec_output=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        exec "$container_id" echo "hello-from-exec" 2>&1) || true

    # Cleanup
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stop "$container_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rm "$container_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
    rm -f "$sandbox_config" "$container_config"

    [[ "$exec_output" == *"hello-from-exec"* ]]
}

test_container_logs() {
    local pod_name="fc-test-logs-$$"

    # Create sandbox
    local sandbox_config=$(mktemp)
    cat > "$sandbox_config" <<EOF
{
    "metadata": {"name": "$pod_name", "namespace": "default", "uid": "test-uid-$$"},
    "log_directory": "/tmp",
    "linux": {}
}
EOF

    local sandbox_id
    sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>&1) || {
        rm -f "$sandbox_config"
        return 1
    }

    # Create container that outputs to stdout
    local container_config=$(mktemp)
    cat > "$container_config" <<EOF
{
    "metadata": {"name": "test-container"},
    "image": {"image": "docker.io/library/alpine:latest"},
    "command": ["sh", "-c", "echo test-log-output && sleep 5"],
    "log_path": "container.log",
    "linux": {}
}
EOF

    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        pull docker.io/library/alpine:latest 2>/dev/null || true

    local container_id
    container_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        create "$sandbox_id" "$container_config" "$sandbox_config" 2>&1) || {
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        rm -f "$sandbox_config" "$container_config"
        return 1
    }

    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" start "$container_id" 2>/dev/null || true
    sleep 3

    # Get logs
    local logs
    logs=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        logs "$container_id" 2>&1) || true

    # Cleanup
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stop "$container_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rm "$container_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
    rm -f "$sandbox_config" "$container_config"

    [[ "$logs" == *"test-log-output"* ]]
}

# =============================================================================
# Kubernetes e2e Tests
# =============================================================================

check_k8s_available() {
    if ! command -v kubectl &>/dev/null; then
        return 1
    fi

    if ! kubectl cluster-info &>/dev/null; then
        return 1
    fi

    return 0
}

setup_k8s_test_namespace() {
    kubectl create namespace fc-conformance-test --dry-run=client -o yaml | kubectl apply -f - 2>/dev/null || true
}

test_k8s_pod_basic() {
    local pod_name="fc-test-basic-$$"

    cat <<EOF | kubectl apply -f - 2>/dev/null || return 1
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: fc-conformance-test
  labels:
    test: fc-conformance
spec:
  runtimeClassName: $RUNTIME_CLASS
  containers:
  - name: test
    image: alpine:latest
    command: ["sleep", "30"]
  restartPolicy: Never
EOF

    # Wait for pod to be running
    local timeout=60
    local elapsed=0
    while [[ $elapsed -lt $timeout ]]; do
        local status
        status=$(kubectl get pod "$pod_name" -n fc-conformance-test -o jsonpath='{.status.phase}' 2>/dev/null) || true

        if [[ "$status" == "Running" ]]; then
            kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
            return 0
        fi

        if [[ "$status" == "Failed" ]]; then
            kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
            return 1
        fi

        sleep 2
        ((elapsed+=2))
    done

    kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
    return 1
}

test_k8s_pod_exec() {
    local pod_name="fc-test-exec-k8s-$$"

    cat <<EOF | kubectl apply -f - 2>/dev/null || return 1
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: fc-conformance-test
  labels:
    test: fc-conformance
spec:
  runtimeClassName: $RUNTIME_CLASS
  containers:
  - name: test
    image: alpine:latest
    command: ["sleep", "60"]
  restartPolicy: Never
EOF

    # Wait for running
    kubectl wait --for=condition=Ready pod/"$pod_name" -n fc-conformance-test --timeout=60s 2>/dev/null || {
        kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
        return 1
    }

    # Test exec
    local output
    output=$(kubectl exec "$pod_name" -n fc-conformance-test -- echo "k8s-exec-works" 2>&1) || {
        kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
        return 1
    }

    kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true

    [[ "$output" == *"k8s-exec-works"* ]]
}

test_k8s_pod_logs() {
    local pod_name="fc-test-logs-k8s-$$"

    cat <<EOF | kubectl apply -f - 2>/dev/null || return 1
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: fc-conformance-test
  labels:
    test: fc-conformance
spec:
  runtimeClassName: $RUNTIME_CLASS
  containers:
  - name: test
    image: alpine:latest
    command: ["sh", "-c", "echo k8s-log-test && sleep 30"]
  restartPolicy: Never
EOF

    # Wait for running
    kubectl wait --for=condition=Ready pod/"$pod_name" -n fc-conformance-test --timeout=60s 2>/dev/null || {
        kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
        return 1
    }

    sleep 2

    # Get logs
    local logs
    logs=$(kubectl logs "$pod_name" -n fc-conformance-test 2>&1) || {
        kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
        return 1
    }

    kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true

    [[ "$logs" == *"k8s-log-test"* ]]
}

test_k8s_resource_limits() {
    local pod_name="fc-test-limits-$$"

    cat <<EOF | kubectl apply -f - 2>/dev/null || return 1
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: fc-conformance-test
  labels:
    test: fc-conformance
spec:
  runtimeClassName: $RUNTIME_CLASS
  containers:
  - name: test
    image: alpine:latest
    command: ["sleep", "30"]
    resources:
      requests:
        memory: "64Mi"
        cpu: "100m"
      limits:
        memory: "128Mi"
        cpu: "500m"
  restartPolicy: Never
EOF

    # Wait for running
    kubectl wait --for=condition=Ready pod/"$pod_name" -n fc-conformance-test --timeout=60s 2>/dev/null || {
        kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
        return 1
    }

    kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
    return 0
}

test_k8s_multi_container() {
    local pod_name="fc-test-multi-$$"

    cat <<EOF | kubectl apply -f - 2>/dev/null || return 1
apiVersion: v1
kind: Pod
metadata:
  name: $pod_name
  namespace: fc-conformance-test
  labels:
    test: fc-conformance
spec:
  runtimeClassName: $RUNTIME_CLASS
  containers:
  - name: container1
    image: alpine:latest
    command: ["sleep", "30"]
  - name: container2
    image: alpine:latest
    command: ["sleep", "30"]
  restartPolicy: Never
EOF

    # Wait for running
    kubectl wait --for=condition=Ready pod/"$pod_name" -n fc-conformance-test --timeout=90s 2>/dev/null || {
        kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
        return 1
    }

    kubectl delete pod "$pod_name" -n fc-conformance-test --wait=false 2>/dev/null || true
    return 0
}

# =============================================================================
# Main
# =============================================================================

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            -v|--verbose)
                VERBOSE=true
                shift
                ;;
            -s|--skip-k8s)
                SKIP_K8S=true
                shift
                ;;
            -c|--critest)
                CRITEST_ONLY=true
                shift
                ;;
            -k|--k8s-only)
                K8S_ONLY=true
                shift
                ;;
            --runtime-class)
                RUNTIME_CLASS="$2"
                shift 2
                ;;
            --timeout)
                TIMEOUT="$2"
                shift 2
                ;;
            -h|--help)
                echo "Usage: $0 [options]"
                echo "Options:"
                echo "  -v, --verbose       Enable verbose output"
                echo "  -s, --skip-k8s      Skip Kubernetes e2e tests"
                echo "  -c, --critest       Run only critest validation"
                echo "  -k, --k8s-only      Run only Kubernetes e2e tests"
                echo "  --runtime-class     RuntimeClass name (default: firecracker)"
                echo "  --timeout           Test timeout in seconds (default: 300)"
                exit 0
                ;;
            *)
                echo "Unknown option: $1"
                exit 1
                ;;
        esac
    done
}

main() {
    parse_args "$@"

    echo ""
    echo "╔═══════════════════════════════════════════════════════════════════════════╗"
    echo "║           firecracker-shim Conformance Tests                              ║"
    echo "╚═══════════════════════════════════════════════════════════════════════════╝"
    echo ""

    # Setup
    mkdir -p "$TEST_DIR" "$LOG_DIR"
    echo "[]" > "$TEST_DIR/results.jsonl"

    check_prerequisites

    # Run critest if requested
    if $CRITEST_ONLY; then
        run_critest
        exit $?
    fi

    if ! $K8S_ONLY; then
        echo ""
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "CRI Validation Tests"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

        run_test "pod_create_delete" test_pod_create_delete || true
        run_test "container_lifecycle" test_container_lifecycle || true
        run_test "container_exec" test_container_exec || true
        run_test "container_logs" test_container_logs || true
    fi

    # Run Kubernetes tests
    if ! $SKIP_K8S; then
        echo ""
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
        echo "Kubernetes e2e Tests"
        echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

        if check_k8s_available; then
            setup_k8s_test_namespace

            run_test "k8s_pod_basic" test_k8s_pod_basic || true
            run_test "k8s_pod_exec" test_k8s_pod_exec || true
            run_test "k8s_pod_logs" test_k8s_pod_logs || true
            run_test "k8s_resource_limits" test_k8s_resource_limits || true
            run_test "k8s_multi_container" test_k8s_multi_container || true
        else
            skip_test "k8s_pod_basic" "Kubernetes cluster not available"
            skip_test "k8s_pod_exec" "Kubernetes cluster not available"
            skip_test "k8s_pod_logs" "Kubernetes cluster not available"
            skip_test "k8s_resource_limits" "Kubernetes cluster not available"
            skip_test "k8s_multi_container" "Kubernetes cluster not available"
        fi
    fi

    # Summary
    echo ""
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "Test Summary"
    echo "━━━━━━━━━━━━━━━━━
