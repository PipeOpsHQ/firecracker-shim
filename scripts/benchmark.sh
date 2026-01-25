#!/bin/bash
# Performance benchmarking script for firecracker-shim
#
# This script measures and documents actual performance numbers:
# - Cold start latency (VM creation → container running)
# - Warm start latency (from VM pool)
# - Memory usage per VM
# - Container operation latencies (create, start, stop, delete)
# - Pool efficiency (hit rate, warming time)
# - Throughput (containers per second)
#
# Prerequisites:
# - firecracker-shim installed and configured
# - containerd running with firecracker runtime
# - Root privileges
# - hyperfine (optional, for more accurate timing)
#
# Usage:
#   sudo ./scripts/benchmark.sh [options]
#
# Options:
#   -n, --iterations N    Number of iterations per test (default: 10)
#   -o, --output FILE     Output results to JSON file
#   -q, --quick           Quick mode (fewer iterations)
#   -v, --verbose         Verbose output
#   --runtime-class NAME  RuntimeClass name (default: firecracker)
#   --skip-warmup         Skip warmup runs
#   --skip-pool           Skip pool benchmarks
#   --skip-throughput     Skip throughput tests

set -euo pipefail

# =============================================================================
# Configuration
# =============================================================================

SCRIPT_DIR="$(dirname "$(realpath "$0")")"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"
RESULTS_DIR="/tmp/fc-benchmark-results"
TIMESTAMP=$(date +%Y%m%d_%H%M%S)

# Defaults
ITERATIONS=10
OUTPUT_FILE=""
QUICK_MODE=false
VERBOSE=false
RUNTIME_CLASS="firecracker"
CONTAINERD_SOCKET="/run/containerd/containerd.sock"
SKIP_WARMUP=false
SKIP_POOL=false
SKIP_THROUGHPUT=false

# Test image
TEST_IMAGE="docker.io/library/alpine:latest"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
BOLD='\033[1m'
NC='\033[0m'

# Results storage
declare -A RESULTS

# =============================================================================
# Utility Functions
# =============================================================================

log() {
    echo -e "${BLUE}[BENCH]${NC} $*"
}

log_verbose() {
    if $VERBOSE; then
        echo -e "${CYAN}[DEBUG]${NC} $*"
    fi
}

log_result() {
    echo -e "${GREEN}[RESULT]${NC} $*"
}

log_header() {
    echo ""
    echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${BOLD}  $*${NC}"
    echo -e "${BOLD}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
}

cleanup() {
    log "Cleaning up benchmark resources..."

    # Kill any test sandboxes
    for sandbox in $(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" pods -q 2>/dev/null | head -20); do
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox" 2>/dev/null || true
    done
}

trap cleanup EXIT

# Get current timestamp in nanoseconds
now_ns() {
    date +%s%N
}

# Convert nanoseconds to milliseconds
ns_to_ms() {
    echo "scale=2; $1 / 1000000" | bc
}

# Calculate statistics from array of values
calc_stats() {
    local -n arr=$1
    local sum=0
    local min=${arr[0]}
    local max=${arr[0]}

    for val in "${arr[@]}"; do
        sum=$(echo "$sum + $val" | bc)
        if (( $(echo "$val < $min" | bc -l) )); then
            min=$val
        fi
        if (( $(echo "$val > $max" | bc -l) )); then
            max=$val
        fi
    done

    local count=${#arr[@]}
    local avg=$(echo "scale=2; $sum / $count" | bc)

    # Calculate std dev
    local sq_sum=0
    for val in "${arr[@]}"; do
        local diff=$(echo "$val - $avg" | bc)
        sq_sum=$(echo "$sq_sum + ($diff * $diff)" | bc)
    done
    local variance=$(echo "scale=4; $sq_sum / $count" | bc)
    local stddev=$(echo "scale=2; sqrt($variance)" | bc)

    # Sort for percentiles
    local sorted=($(printf '%s\n' "${arr[@]}" | sort -n))
    local p50_idx=$(( count / 2 ))
    local p95_idx=$(( count * 95 / 100 ))
    local p99_idx=$(( count * 99 / 100 ))

    local p50=${sorted[$p50_idx]}
    local p95=${sorted[$p95_idx]:-${sorted[-1]}}
    local p99=${sorted[$p99_idx]:-${sorted[-1]}}

    echo "$avg $min $max $stddev $p50 $p95 $p99"
}

# =============================================================================
# System Information
# =============================================================================

collect_system_info() {
    log_header "System Information"

    echo "Hostname:      $(hostname)"
    echo "Kernel:        $(uname -r)"
    echo "CPU:           $(grep 'model name' /proc/cpuinfo | head -1 | cut -d: -f2 | xargs)"
    echo "CPU Cores:     $(nproc)"
    echo "Memory:        $(free -h | grep Mem | awk '{print $2}')"
    echo "Firecracker:   $(firecracker --version 2>&1 | head -1 || echo 'not found')"
    echo "containerd:    $(containerd --version 2>&1 | head -1 || echo 'not found')"
    echo ""

    # Store for JSON output
    RESULTS[hostname]=$(hostname)
    RESULTS[kernel]=$(uname -r)
    RESULTS[cpu_cores]=$(nproc)
    RESULTS[memory_gb]=$(free -g | grep Mem | awk '{print $2}')
}

# =============================================================================
# Prerequisite Checks
# =============================================================================

check_prerequisites() {
    log "Checking prerequisites..."

    if [[ $EUID -ne 0 ]]; then
        echo "This script must be run as root"
        exit 1
    fi

    if [[ ! -S "$CONTAINERD_SOCKET" ]]; then
        echo "containerd socket not found at $CONTAINERD_SOCKET"
        exit 1
    fi

    if ! command -v crictl &>/dev/null; then
        echo "crictl not found"
        exit 1
    fi

    if ! command -v bc &>/dev/null; then
        echo "bc not found (required for calculations)"
        exit 1
    fi

    # Pull test image
    log "Pulling test image: $TEST_IMAGE"
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" pull "$TEST_IMAGE" >/dev/null 2>&1 || true

    log "Prerequisites satisfied"
}

# =============================================================================
# Warmup
# =============================================================================

run_warmup() {
    if $SKIP_WARMUP; then
        return
    fi

    log_header "Warmup"
    log "Running warmup iterations..."

    for i in {1..3}; do
        log_verbose "Warmup iteration $i"

        local sandbox_config=$(mktemp)
        cat > "$sandbox_config" <<EOF
{"metadata":{"name":"warmup-$i","namespace":"default","uid":"warmup-$$-$i"},"log_directory":"/tmp","linux":{}}
EOF

        local sandbox_id
        sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>/dev/null) || true

        if [[ -n "$sandbox_id" ]]; then
            crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
            crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        fi

        rm -f "$sandbox_config"
    done

    log "Warmup complete"
}

# =============================================================================
# Cold Start Benchmark
# =============================================================================

benchmark_cold_start() {
    log_header "Cold Start Latency"
    log "Measuring time from sandbox creation to container running (no pool)"

    local latencies=()

    for i in $(seq 1 $ITERATIONS); do
        log_verbose "Iteration $i/$ITERATIONS"

        local sandbox_config=$(mktemp)
        local container_config=$(mktemp)

        cat > "$sandbox_config" <<EOF
{"metadata":{"name":"bench-cold-$i","namespace":"default","uid":"bench-cold-$$-$i"},"log_directory":"/tmp","linux":{}}
EOF

        cat > "$container_config" <<EOF
{"metadata":{"name":"test"},"image":{"image":"$TEST_IMAGE"},"command":["sleep","10"],"log_path":"test.log","linux":{}}
EOF

        local start_time=$(now_ns)

        # Create sandbox
        local sandbox_id
        sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>/dev/null) || {
            rm -f "$sandbox_config" "$container_config"
            continue
        }

        # Create container
        local container_id
        container_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            create "$sandbox_id" "$container_config" "$sandbox_config" 2>/dev/null) || {
            crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
            crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
            rm -f "$sandbox_config" "$container_config"
            continue
        }

        # Start container
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            start "$container_id" 2>/dev/null || true

        local end_time=$(now_ns)
        local latency_ms=$(ns_to_ms $((end_time - start_time)))
        latencies+=("$latency_ms")

        log_verbose "  Latency: ${latency_ms}ms"

        # Cleanup
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stop "$container_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rm "$container_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true

        rm -f "$sandbox_config" "$container_config"
    done

    if [[ ${#latencies[@]} -gt 0 ]]; then
        local stats=($(calc_stats latencies))
        local avg=${stats[0]}
        local min=${stats[1]}
        local max=${stats[2]}
        local stddev=${stats[3]}
        local p50=${stats[4]}
        local p95=${stats[5]}
        local p99=${stats[6]}

        echo ""
        echo "Results (${#latencies[@]} samples):"
        echo "  Average:  ${avg}ms"
        echo "  Min:      ${min}ms"
        echo "  Max:      ${max}ms"
        echo "  Std Dev:  ${stddev}ms"
        echo "  P50:      ${p50}ms"
        echo "  P95:      ${p95}ms"
        echo "  P99:      ${p99}ms"

        RESULTS[cold_start_avg]=$avg
        RESULTS[cold_start_min]=$min
        RESULTS[cold_start_max]=$max
        RESULTS[cold_start_p50]=$p50
        RESULTS[cold_start_p95]=$p95
        RESULTS[cold_start_p99]=$p99
    else
        echo "No successful measurements"
    fi
}

# =============================================================================
# Container Operation Benchmarks
# =============================================================================

benchmark_container_ops() {
    log_header "Container Operation Latencies"

    # Create a sandbox to use for all container operations
    local sandbox_config=$(mktemp)
    cat > "$sandbox_config" <<EOF
{"metadata":{"name":"bench-ops","namespace":"default","uid":"bench-ops-$$"},"log_directory":"/tmp","linux":{}}
EOF

    local sandbox_id
    sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>/dev/null) || {
        rm -f "$sandbox_config"
        log "Failed to create sandbox for container ops benchmark"
        return
    }

    local create_latencies=()
    local start_latencies=()
    local stop_latencies=()
    local delete_latencies=()

    for i in $(seq 1 $ITERATIONS); do
        log_verbose "Iteration $i/$ITERATIONS"

        local container_config=$(mktemp)
        cat > "$container_config" <<EOF
{"metadata":{"name":"test-$i"},"image":{"image":"$TEST_IMAGE"},"command":["sleep","30"],"log_path":"test.log","linux":{}}
EOF

        # Measure create
        local start_time=$(now_ns)
        local container_id
        container_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            create "$sandbox_id" "$container_config" "$sandbox_config" 2>/dev/null) || {
            rm -f "$container_config"
            continue
        }
        local end_time=$(now_ns)
        create_latencies+=("$(ns_to_ms $((end_time - start_time)))")

        # Measure start
        start_time=$(now_ns)
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            start "$container_id" 2>/dev/null || true
        end_time=$(now_ns)
        start_latencies+=("$(ns_to_ms $((end_time - start_time)))")

        sleep 1

        # Measure stop
        start_time=$(now_ns)
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            stop "$container_id" 2>/dev/null || true
        end_time=$(now_ns)
        stop_latencies+=("$(ns_to_ms $((end_time - start_time)))")

        # Measure delete
        start_time=$(now_ns)
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            rm "$container_id" 2>/dev/null || true
        end_time=$(now_ns)
        delete_latencies+=("$(ns_to_ms $((end_time - start_time)))")

        rm -f "$container_config"
    done

    # Cleanup sandbox
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
    rm -f "$sandbox_config"

    # Print results
    echo ""
    echo "Container Create (${#create_latencies[@]} samples):"
    if [[ ${#create_latencies[@]} -gt 0 ]]; then
        local stats=($(calc_stats create_latencies))
        echo "  Average: ${stats[0]}ms  P50: ${stats[4]}ms  P95: ${stats[5]}ms"
        RESULTS[container_create_avg]=${stats[0]}
        RESULTS[container_create_p95]=${stats[5]}
    fi

    echo ""
    echo "Container Start (${#start_latencies[@]} samples):"
    if [[ ${#start_latencies[@]} -gt 0 ]]; then
        local stats=($(calc_stats start_latencies))
        echo "  Average: ${stats[0]}ms  P50: ${stats[4]}ms  P95: ${stats[5]}ms"
        RESULTS[container_start_avg]=${stats[0]}
        RESULTS[container_start_p95]=${stats[5]}
    fi

    echo ""
    echo "Container Stop (${#stop_latencies[@]} samples):"
    if [[ ${#stop_latencies[@]} -gt 0 ]]; then
        local stats=($(calc_stats stop_latencies))
        echo "  Average: ${stats[0]}ms  P50: ${stats[4]}ms  P95: ${stats[5]}ms"
        RESULTS[container_stop_avg]=${stats[0]}
        RESULTS[container_stop_p95]=${stats[5]}
    fi

    echo ""
    echo "Container Delete (${#delete_latencies[@]} samples):"
    if [[ ${#delete_latencies[@]} -gt 0 ]]; then
        local stats=($(calc_stats delete_latencies))
        echo "  Average: ${stats[0]}ms  P50: ${stats[4]}ms  P95: ${stats[5]}ms"
        RESULTS[container_delete_avg]=${stats[0]}
        RESULTS[container_delete_p95]=${stats[5]}
    fi
}

# =============================================================================
# Memory Benchmark
# =============================================================================

benchmark_memory() {
    log_header "Memory Usage"

    # Create a sandbox and measure memory
    local sandbox_config=$(mktemp)
    cat > "$sandbox_config" <<EOF
{"metadata":{"name":"bench-mem","namespace":"default","uid":"bench-mem-$$"},"log_directory":"/tmp","linux":{}}
EOF

    local sandbox_id
    sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
        runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>/dev/null) || {
        rm -f "$sandbox_config"
        log "Failed to create sandbox for memory benchmark"
        return
    }

    # Get firecracker process
    local fc_pids=$(pgrep -f "firecracker.*$sandbox_id" || pgrep firecracker | tail -1)

    if [[ -n "$fc_pids" ]]; then
        for pid in $fc_pids; do
            if [[ -f "/proc/$pid/status" ]]; then
                local vmrss=$(grep VmRSS /proc/$pid/status 2>/dev/null | awk '{print $2}')
                local vmpeak=$(grep VmPeak /proc/$pid/status 2>/dev/null | awk '{print $2}')

                if [[ -n "$vmrss" ]]; then
                    local vmrss_mb=$(echo "scale=2; $vmrss / 1024" | bc)
                    echo "Firecracker Process (PID $pid):"
                    echo "  Resident Memory: ${vmrss_mb}MB"

                    RESULTS[memory_resident_mb]=$vmrss_mb

                    if [[ -n "$vmpeak" ]]; then
                        local vmpeak_mb=$(echo "scale=2; $vmpeak / 1024" | bc)
                        echo "  Peak Memory:     ${vmpeak_mb}MB"
                        RESULTS[memory_peak_mb]=$vmpeak_mb
                    fi
                fi
                break
            fi
        done
    else
        echo "Could not find Firecracker process"
    fi

    # Cleanup
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
    crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
    rm -f "$sandbox_config"
}

# =============================================================================
# Pool Benchmark
# =============================================================================

benchmark_pool() {
    if $SKIP_POOL; then
        return
    fi

    log_header "VM Pool Performance"

    # Check if metrics endpoint is available
    local metrics_url="http://localhost:9090/metrics"
    if ! curl -s "$metrics_url" &>/dev/null; then
        log "Metrics endpoint not available, skipping pool benchmark"
        return
    fi

    # Get current pool stats
    local metrics=$(curl -s "$metrics_url")

    local pool_available=$(echo "$metrics" | grep "^fc_cri_pool_available " | awk '{print $2}')
    local pool_hits=$(echo "$metrics" | grep "^fc_cri_pool_hits_total " | awk '{print $2}')
    local pool_misses=$(echo "$metrics" | grep "^fc_cri_pool_misses_total " | awk '{print $2}')
    local hit_rate=$(echo "$metrics" | grep "^fc_cri_pool_hit_rate " | awk '{print $2}')

    echo "Pool Status:"
    echo "  Available VMs: ${pool_available:-N/A}"
    echo "  Total Hits:    ${pool_hits:-N/A}"
    echo "  Total Misses:  ${pool_misses:-N/A}"
    echo "  Hit Rate:      ${hit_rate:-N/A}%"

    RESULTS[pool_available]=${pool_available:-0}
    RESULTS[pool_hit_rate]=${hit_rate:-0}

    # Measure warm start (from pool)
    if [[ -n "$pool_available" ]] && [[ "$pool_available" -gt 0 ]]; then
        echo ""
        echo "Measuring warm start from pool..."

        local warm_latencies=()

        for i in $(seq 1 $ITERATIONS); do
            local sandbox_config=$(mktemp)
            cat > "$sandbox_config" <<EOF
{"metadata":{"name":"bench-warm-$i","namespace":"default","uid":"bench-warm-$$-$i"},"log_directory":"/tmp","linux":{}}
EOF

            local start_time=$(now_ns)

            local sandbox_id
            sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
                runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>/dev/null) || {
                rm -f "$sandbox_config"
                continue
            }

            local end_time=$(now_ns)
            warm_latencies+=("$(ns_to_ms $((end_time - start_time)))")

            # Quick cleanup to return to pool
            crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
            crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
            rm -f "$sandbox_config"

            # Small delay to allow pool replenishment
            sleep 0.5
        done

        if [[ ${#warm_latencies[@]} -gt 0 ]]; then
            local stats=($(calc_stats warm_latencies))
            echo ""
            echo "Warm Start Results (${#warm_latencies[@]} samples):"
            echo "  Average: ${stats[0]}ms"
            echo "  P50:     ${stats[4]}ms"
            echo "  P95:     ${stats[5]}ms"

            RESULTS[warm_start_avg]=${stats[0]}
            RESULTS[warm_start_p50]=${stats[4]}
            RESULTS[warm_start_p95]=${stats[5]}
        fi
    fi
}

# =============================================================================
# Throughput Benchmark
# =============================================================================

benchmark_throughput() {
    if $SKIP_THROUGHPUT; then
        return
    fi

    log_header "Throughput Test"
    log "Creating containers as fast as possible for 30 seconds..."

    local duration=30
    local count=0
    local start_time=$(date +%s)
    local end_time=$((start_time + duration))

    while [[ $(date +%s) -lt $end_time ]]; do
        local sandbox_config=$(mktemp)
        cat > "$sandbox_config" <<EOF
{"metadata":{"name":"bench-tp-$count","namespace":"default","uid":"bench-tp-$$-$count"},"log_directory":"/tmp","linux":{}}
EOF

        local sandbox_id
        sandbox_id=$(crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" \
            runp --runtime "$RUNTIME_CLASS" "$sandbox_config" 2>/dev/null) || {
            rm -f "$sandbox_config"
            continue
        }

        ((count++))

        # Cleanup immediately
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" stopp "$sandbox_id" 2>/dev/null || true
        crictl --runtime-endpoint "unix://$CONTAINERD_SOCKET" rmp "$sandbox_id" 2>/dev/null || true
        rm -f "$sandbox_config"

        if $VERBOSE; then
            echo -ne "\r  Created: $count sandboxes"
        fi
    done

    echo ""
    local actual_duration=$(($(date +%s) - start_time))
    local rate=$(echo "scale=2; $count / $actual_duration" | bc)

    echo "Results:"
    echo "  Duration:   ${actual_duration}s"
    echo "  Sandboxes:  $count"
    echo "  Throughput: ${rate} sandboxes/second"

    RESULTS[throughput_sandboxes]=$count
    RESULTS[throughput_rate]=$rate
}

# =============================================================================
# Output Results
# =============================================================================

output_results() {
    log_header "Summary"

    echo "Key Metrics:"
    echo "  Cold Start (avg):    ${RESULTS[cold_start_avg]:-N/A}ms"
    echo "  Cold Start (P95):    ${RESULTS[cold_start_p95]:-N/A}ms"
    echo "  Warm Start (avg):    ${RESULTS[warm_start_avg]:-N/A}ms"
    echo "  Memory (resident):   ${RESULTS[memory_resident_mb]:-N/A}MB"
    echo "  Pool Hit Rate:       ${RESULTS[pool_hit_rate]:-N/A}%"
    echo "  Throughput:          ${RESULTS[throughput_rate]:-N/A} sandboxes/s"

    # Output JSON if requested
    if [[ -n "$OUTPUT_FILE" ]]; then
        log "Writing results to $OUTPUT_FILE"

        cat > "$OUTPUT_FILE" <<EOF
{
  "timestamp": "$(date -Iseconds)",
  "system": {
    "hostname": "${RESULTS[hostname]:-}",
    "kernel": "${RESULTS[kernel]:-}",
    "cpu_cores": ${RESULTS[cpu_cores]:-0},
    "memory_gb": ${RESULTS[memory_gb]:-0}
  },
  "cold_start": {
    "avg_ms": ${RESULTS[cold_start_avg]:-null},
    "min_ms": ${RESULTS[cold_start_min]:-null},
    "max_ms": ${RESULTS[cold_start_max]:-null},
    "p50_ms": ${RESULTS[cold_start_p50]:-null},
    "p95_ms": ${RESULTS[cold_start_p95]:-null},
    "p99_ms": ${RESULTS[cold_start_p99]:-null}
  },
  "warm_start": {
    "avg_ms": ${RESULTS[warm_start_avg]:-null},
    "p50_ms": ${RESULTS[warm_start_p50]:-null},
    "p95_ms": ${RESULTS[warm_start_p95]:-null}
  },
  "container_ops": {
    "create_avg_ms": ${RESULTS[container_create_avg]:-null},
    "start_avg_ms": ${RESULTS[container_start_avg]:-null},
    "stop_avg_ms": ${RESULTS[container_stop_avg]:-null},
    "delete_avg_ms": ${RESULTS[container_delete_avg]:-null}
  },
  "memory": {
    "resident_mb": ${RESULTS[memory_resident_mb]:-null},
    "peak_mb": ${RESULTS[memory_peak_mb]:-null}
  },
  "pool": {
    "available": ${RESULTS[pool_available]:-null},
    "hit_rate_pct": ${RESULTS[pool_hit_rate]:-null}
  },
  "throughput": {
    "sandboxes_total": ${RESULTS[throughput_sandboxes]:-null},
    "rate_per_second": ${RESULTS[throughput_rate]:-null}
  },
  "config": {
    "iterations": $ITERATIONS,
    "runtime_class": "$RUNTIME_CLASS",
    "test_image": "$TEST_IMAGE"
  }
}
EOF

        echo ""
        log_result "Results saved to $OUTPUT_FILE"
    fi
}

# =============================================================================
# Main
# =============================================================================

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            -n|--iterations)
                ITERATIONS="$2"
                shift 2
                ;;
            -o|--output)
                OUTPUT_FILE="$2"
                shift 2
                ;;
            -q|--quick)
                QUICK_MODE=true
                ITERATIONS=5
                shift
                ;;
            -v|--verbose)
                VERBOSE=true
                shift
                ;;
            --runtime-class)
                RUNTIME_CLASS="$2"
                shift 2
                ;;
            --skip-warmup)
                SKIP_WARMUP=true
                shift
                ;;
            --skip-pool)
                SKIP_POOL=true
                shift
                ;;
            --skip-throughput)
                SKIP_THROUGHPUT=true
                shift
                ;;
            -h|--help)
                echo "Usage: $0 [options]"
                echo ""
                echo "Options:"
                echo "  -n, --iterations N    Number of iterations (default: 10)"
                echo "  -o, --output FILE     Output JSON file"
                echo "  -q, --quick           Quick mode (5 iterations)"
                echo "  -v, --verbose         Verbose output"
                echo "  --runtime-class NAME  RuntimeClass (default: firecracker)"
                echo "  --skip-warmup         Skip warmup runs"
                echo "  --skip-pool           Skip pool benchmarks"
                echo "  --skip-throughput     Skip throughput tests"
                echo "  -h, --help            Show this help"
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
    echo -e "${BOLD}╔════════════════════════════════════════════════════════════════════════════╗${NC}"
    echo -e "${BOLD}║                firecracker-shim Performance Benchmark                      ║${NC}"
    echo -e "${BOLD}╚════════════════════════════════════════════════════════════════════════════╝${NC}"
    echo ""

    check_prerequisites
    collect_system_info

    run_warmup

    benchmark_cold_start
    benchmark_container_ops
    benchmark_memory
    benchmark_pool
    benchmark_throughput

    output_results
}

main "$@"
