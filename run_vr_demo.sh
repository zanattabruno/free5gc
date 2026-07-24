#!/bin/bash
# VR Demo — Launch NWDAF, VR Simulator, and Dashboard
# Usage: ./run_vr_demo.sh [--sessions N] [--profile PROFILE]
set -e

BASEDIR="$(cd "$(dirname "$0")" && pwd)"
SESSIONS=3
PROFILE="INTERACTIVE_VR"

while [[ $# -gt 0 ]]; do
    case $1 in
        --sessions) SESSIONS="$2"; shift 2 ;;
        --profile)  PROFILE="$2"; shift 2 ;;
        *) shift ;;
    esac
done

echo "============================================"
echo "  5G VR Network Demo"
echo "============================================"
echo "  Sessions: $SESSIONS | Profile: $PROFILE"
echo ""

PID_LIST=()

cleanup() {
    echo ""
    echo "Shutting down VR demo..."
    for pid in "${PID_LIST[@]}"; do
        kill "$pid" 2>/dev/null || true
    done
    wait "${PID_LIST[@]}" 2>/dev/null
    echo "VR demo stopped."
    exit 0
}
trap cleanup SIGINT SIGTERM

# 1. Check NWDAF (started by run.sh)
if pgrep -f "bin/nwdaf" > /dev/null 2>&1; then
    echo "[1/2] NWDAF already running (started by run.sh) ✓"
else
    echo "[1/2] Starting NWDAF (127.0.0.15:8000)..."
    "${BASEDIR}/bin/nwdaf" -c "${BASEDIR}/config/nwdafcfg.yaml" &
    PID_LIST+=($!)
    sleep 1
fi

# 2. Start VR Simulator + Dashboard
echo "[2/3] Starting VR Simulator (API :9090, Dashboard :3000)..."
"${BASEDIR}/bin/ue-vr-sim" -c "${BASEDIR}/../ue-vr-sim/config/vrsimcfg.yaml" &
PID_LIST+=($!)
sleep 1

# 3. Info
echo "[3/3] All services running!"
echo ""
echo "============================================"
echo "  NWDAF:      http://127.0.0.15:8000"
echo "  VR Sim API: http://localhost:9090/api/sessions"
echo "  Dashboard:  http://localhost:3000"
echo "============================================"
echo ""
echo "Press Ctrl+C to stop all services."

wait "${PID_LIST[@]}"
