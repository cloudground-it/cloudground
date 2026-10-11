#!/bin/bash
# Managed by CloudGround: after a renewal, reload nginx to serve the new certificate.
systemctl reload nginx 2>/dev/null || true
