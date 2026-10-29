#!/usr/bin/env bash
# ==============================================================================
# CloudPulse Telemetry - End-to-End Incident Simulation & Load Testing
# ==============================================================================
set -euo pipefail

CP_URL="${CONTROL_PLANE_URL:-http://localhost:8000}"
COLLECTOR_URL="${COLLECTOR_URL:-http://localhost:9100}"
NODE_NAME="${SIMULATED_NODE:-$(hostname)}"
DURATION_SECS="${STRESS_DURATION:-5}"

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_ok() {
    echo -e "${GREEN}[OK]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

# ------------------------------------------------------------------------------
# 1. Verify Local Stack Availability
# ------------------------------------------------------------------------------
log_info "Step 1: Verifying platform health..."

check_service() {
    local url="$1"
    local name="$2"
    log_info "Checking ${name} at ${url}..."
    if ! curl -sf --max-time 3 "${url}" > /dev/null; then
        log_error "${name} is not reachable at ${url}."
        log_error "Ensure services are running (e.g. 'make docker-up' or 'docker compose -f deployments/docker-compose.yml up -d')."
        return 1
    fi
    log_ok "${name} is healthy."
}

if ! check_service "${CP_URL}/healthz/live" "Control-Plane Live Probe"; then
    exit 1
fi

if ! check_service "${COLLECTOR_URL}/metrics" "Collector Daemon Metrics"; then
    exit 1
fi

# ------------------------------------------------------------------------------
# 2. Simulate Artificial CPU and Memory Stress
# ------------------------------------------------------------------------------
log_info "Step 2: Simulating artificial CPU and memory pressure for ${DURATION_SECS} seconds..."

cleanup_stress() {
    if [[ -n "${STRESS_PID:-}" ]] && kill -0 "${STRESS_PID}" 2>/dev/null; then
        kill -9 "${STRESS_PID}" 2>/dev/null || true
    fi
}
trap cleanup_stress EXIT INT TERM

if command -v stress-ng >/dev/null 2>&1; then
    log_info "Using stress-ng for load generation..."
    stress-ng --cpu 2 --vm 1 --vm-bytes 128M --timeout "${DURATION_SECS}s" --quiet &
    STRESS_PID=$!
elif command -v stress >/dev/null 2>&1; then
    log_info "Using stress for load generation..."
    stress --cpu 2 --vm 1 --vm-bytes 128M --timeout "${DURATION_SECS}s" &
    STRESS_PID=$!
else
    log_warn "Neither stress-ng nor stress found. Using native bash workload fallback..."
    (
        # Spawn CPU and memory consumers in background
        for _ in 1 2; do
            ( while true; do :; done ) &
        done
        python3 -c "import time; x = bytearray(128 * 1024 * 1024); time.sleep(${DURATION_SECS})" 2>/dev/null || sleep "${DURATION_SECS}"
    ) &
    STRESS_PID=$!
    (
        sleep "${DURATION_SECS}"
        kill -9 "${STRESS_PID}" 2>/dev/null || true
        pkill -P "${STRESS_PID}" 2>/dev/null || true
    ) &
fi

log_info "Workload running (PID: ${STRESS_PID}). Waiting for completion..."
wait "${STRESS_PID}" 2>/dev/null || true
log_ok "Workload simulation finished."

# ------------------------------------------------------------------------------
# 3. Monitor Collector Metrics for Spike & Telemetry Signals
# ------------------------------------------------------------------------------
log_info "Step 3: Inspecting collector /metrics for telemetry exposition..."

METRICS_OUTPUT=$(curl -sf --max-time 3 "${COLLECTOR_URL}/metrics" || true)

if echo "${METRICS_OUTPUT}" | grep -q "cloudpulse_cpu_usage_percent"; then
    CPU_SAMPLE=$(echo "${METRICS_OUTPUT}" | grep -E '^cloudpulse_cpu_usage_percent\{cpu="all"\}' | head -n 1 | awk '{print $2}' || echo "N/A")
    log_ok "Collector CPU metric observed: ${CPU_SAMPLE}%"
else
    log_warn "cloudpulse_cpu_usage_percent metric not yet populated."
fi

if echo "${METRICS_OUTPUT}" | grep -q "cloudpulse_memory_used_percent"; then
    MEM_SAMPLE=$(echo "${METRICS_OUTPUT}" | grep -E '^cloudpulse_memory_used_percent ' | awk '{print $2}' || echo "N/A")
    log_ok "Collector Memory used metric observed: ${MEM_SAMPLE}%"
fi

ANOMALY_COUNT=$(echo "${METRICS_OUTPUT}" | grep -E '^cloudpulse_anomalies_detected_total' | awk '{s+=$2} END {print s+0}')
log_info "Total anomaly events tracked by agent: ${ANOMALY_COUNT}"

# ------------------------------------------------------------------------------
# 4. Dispatch Telemetry and Query AI Root-Cause Diagnosis
# ------------------------------------------------------------------------------
log_info "Step 4: Submitting anomaly incident to control-plane for diagnosis..."

NOW_ISO=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

PAYLOAD=$(cat <<EOF
{
  "node": "${NODE_NAME}",
  "context": "Simulated E2E load spike with stress-ng on host",
  "events": [
    {
      "kind": "cpu_spike",
      "value": 94.2,
      "threshold": 85.0,
      "timestamp": "${NOW_ISO}",
      "details": {
        "load_duration_seconds": ${DURATION_SECS}
      }
    },
    {
      "kind": "memory_pressure",
      "value": 91.8,
      "threshold": 90.0,
      "timestamp": "${NOW_ISO}",
      "details": {
        "swap_activity": 1.0
      }
    }
  ]
}
EOF
)

RESPONSE=$(curl -sf -w "\n%{http_code}" -X POST "${CP_URL}/api/v1/incidents/analyze" \
  -H "Content-Type: application/json" \
  -d "${PAYLOAD}")

HTTP_STATUS=$(echo "${RESPONSE}" | tail -n1)
JSON_BODY=$(echo "${RESPONSE}" | sed '$d')

if [[ "${HTTP_STATUS}" != "200" ]]; then
    log_error "Incident analysis failed with HTTP ${HTTP_STATUS}"
    echo "${JSON_BODY}" >&2
    exit 1
fi

log_ok "Received 200 OK from /api/v1/incidents/analyze"

# Verify structured fields in response
python3 -c "
import sys, json

data = json.loads('''${JSON_BODY}''')
assert 'node' in data, 'Missing node in response'
assert 'source' in data, 'Missing source in response'
assert data['source'] in ('gemini', 'fallback'), f'Unexpected source: {data[\"source\"]}'
assert 'diagnosis' in data, 'Missing diagnosis'
diag = data['diagnosis']
assert 'root_cause' in diag and len(diag['root_cause']) > 0, 'Invalid root_cause'
assert 'severity' in diag, 'Missing severity'
assert 'confidence' in diag, 'Missing confidence'
assert 'recommendations' in diag and len(diag['recommendations']) > 0, 'Missing recommendations'

print('\n' + '='*60)
print('CLOUD-PULSE ROOT CAUSE DIAGNOSIS REPORT')
print('='*60)
print(f'Node:            {data[\"node\"]}')
print(f'Analysis Source: {data[\"source\"]} (degraded_reason: {data.get(\"degraded_reason\")})')
print(f'Severity:        {diag[\"severity\"].upper()}')
print(f'Confidence:      {diag[\"confidence\"] * 100:.1f}%')
print(f'Root Cause:      {diag[\"root_cause\"]}')
print('Remediations:')
for i, rec in enumerate(diag['recommendations'], 1):
    print(f'  {i}. {rec}')
print('='*60 + '\n')
"

log_ok "End-to-End incident simulation completed successfully!"
