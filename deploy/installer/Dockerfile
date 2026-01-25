FROM alpine:3.19

# Install dependencies for script
RUN apk add --no-cache bash

# Create directory structure
RUN mkdir -p /binaries /config /assets

# Copy script
COPY install.sh /install.sh
RUN chmod +x /install.sh

# Note: Binaries and config will be copied in by the CI process
# or can be built in a multi-stage build.
# For local dev, we assume context has dist/ folder.

ENTRYPOINT ["/install.sh"]
