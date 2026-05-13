#!/bin/bash
# NEF (Network Exposure Function) Test Script
# Tests both PFD Management and Traffic Influence APIs

set -e

NEF_BASE="http://127.0.0.5:8000"
AF_ID="testAF01"
PASS=0
FAIL=0

GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

check_result() {
    local test_name="$1"
    local expected_code="$2"
    local actual_code="$3"
    local response="$4"

    if [ "$actual_code" == "$expected_code" ]; then
        echo -e "${GREEN}[PASS]${NC} $test_name (HTTP $actual_code)"
        PASS=$((PASS + 1))
    else
        echo -e "${RED}[FAIL]${NC} $test_name (Expected HTTP $expected_code, got HTTP $actual_code)"
        echo -e "       Response: $response"
        FAIL=$((FAIL + 1))
    fi
}

echo -e "${CYAN}============================================${NC}"
echo -e "${CYAN}   free5GC NEF Test Suite${NC}"
echo -e "${CYAN}============================================${NC}"
echo ""

# ============================================================
# 1. Basic Connectivity
# ============================================================
echo -e "${YELLOW}--- 1. Basic Connectivity ---${NC}"

# Test 1.1: NEF OAM health check
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/nnef-oam/v1/")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "OAM Health Check" "200" "$HTTP_CODE" "$BODY"

# ============================================================
# 2. Nnef_PFDManagement (SBI interface - used by SMF)
# ============================================================
echo ""
echo -e "${YELLOW}--- 2. Nnef_PFDManagement (SBI) ---${NC}"

# Test 2.1: GET applications PFDs (should be empty initially)
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/nnef-pfdmanagement/v1/applications")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET PFD Applications (empty)" "200" "$HTTP_CODE" "$BODY"

# ============================================================
# 3. Traffic Influence API (3GPP TS 29.522)
#    This API auto-creates the AF context
# ============================================================
echo ""
echo -e "${YELLOW}--- 3. Traffic Influence API ---${NC}"

# Test 3.1: GET subscriptions for non-existent AF
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET TI Subscriptions (AF not found)" "404" "$HTTP_CODE" "$BODY"

# Test 3.2: POST Traffic Influence subscription (creates AF + subscription)
# This creates a subscription for "any UE" (anyUeInd=true) which goes to UDR
RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions" \
  -H "Content-Type: application/json" \
  -d '{
    "afAppId": "testApp1",
    "anyUeInd": true,
    "trafficRoutes": [
      {
        "dnai": "edge1",
        "routeInfo": {
          "ipv4Addr": "192.168.1.1",
          "portNumber": 8080
        }
      }
    ],
    "dnn": "internet",
    "snssai": {
      "sst": 1,
      "sd": "010203"
    }
  }')
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "POST TI Subscription (anyUeInd)" "201" "$HTTP_CODE" "$BODY"

# Extract self link and subscription ID if created
if [ "$HTTP_CODE" == "201" ]; then
    TI_SELF=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('self',''))" 2>/dev/null || echo "")
    SUB_ID=$(echo "$TI_SELF" | grep -oP 'subscriptions/\K[^/]+$' || echo "")
    echo -e "  → Created subscription: $TI_SELF"
fi

# Test 3.3: GET all subscriptions for AF (should have 1 now)
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET TI Subscriptions (1 exists)" "200" "$HTTP_CODE" "$BODY"

# Test 3.4: GET individual subscription
if [ -n "$SUB_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions/${SUB_ID}")
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "GET Individual TI Subscription" "200" "$HTTP_CODE" "$BODY"
fi

# Test 3.5: PATCH individual subscription (modify traffic routes)
if [ -n "$SUB_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" -X PATCH "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions/${SUB_ID}" \
      -H "Content-Type: application/json" \
      -d '{
        "trafficRoutes": [
          {
            "dnai": "edge2",
            "routeInfo": {
              "ipv4Addr": "192.168.2.1",
              "portNumber": 9090
            }
          }
        ]
      }')
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "PATCH Individual TI Subscription" "200" "$HTTP_CODE" "$BODY"
fi

# Test 3.6: DELETE individual subscription
if [ -n "$SUB_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" -X DELETE "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions/${SUB_ID}")
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "DELETE Individual TI Subscription" "204" "$HTTP_CODE" "$BODY"
fi

# ============================================================
# 4. PFD Management API (3GPP TS 29.122)
#    AF context was created by the TI subscription above
# ============================================================
echo ""
echo -e "${YELLOW}--- 4. PFD Management API ---${NC}"

# Test 4.1: GET PFD Transactions (AF exists now, should be empty)
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET PFD Transactions (empty)" "200" "$HTTP_CODE" "$BODY"

# Test 4.2: POST PFD Transaction (create PFDs for applications)
RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions" \
  -H "Content-Type: application/json" \
  -d '{
    "pfdDatas": {
      "app1": {
        "externalAppId": "app1",
        "pfds": {
          "pfd1": {
            "pfdId": "pfd1",
            "flowDescriptions": ["permit out ip from 10.0.0.0/24 to any"],
            "urls": ["https://example.com/video"],
            "domainNames": ["example.com"]
          },
          "pfd2": {
            "pfdId": "pfd2",
            "flowDescriptions": ["permit out ip from 172.16.0.0/16 to any"]
          }
        }
      },
      "app2": {
        "externalAppId": "app2",
        "pfds": {
          "pfd3": {
            "pfdId": "pfd3",
            "domainNames": ["streaming.example.com", "cdn.example.com"]
          }
        }
      }
    }
  }')
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "POST PFD Transaction (2 apps)" "201" "$HTTP_CODE" "$BODY"

# Extract transaction info
if [ "$HTTP_CODE" == "201" ]; then
    PFD_SELF=$(echo "$BODY" | python3 -c "import sys,json; print(json.load(sys.stdin).get('self',''))" 2>/dev/null || echo "")
    TRANS_ID=$(echo "$PFD_SELF" | grep -oP 'transactions/\K[^/]+$' || echo "")
    echo -e "  → Created transaction: $PFD_SELF"
fi

# Test 4.3: GET all PFD Transactions
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET PFD Transactions (1 exists)" "200" "$HTTP_CODE" "$BODY"

# Test 4.4: GET individual PFD transaction
if [ -n "$TRANS_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions/${TRANS_ID}")
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "GET Individual PFD Transaction" "200" "$HTTP_CODE" "$BODY"
fi

# Test 4.5: GET individual application PFD
if [ -n "$TRANS_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions/${TRANS_ID}/applications/app1")
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "GET Individual App PFD (app1)" "200" "$HTTP_CODE" "$BODY"
fi

# Test 4.6: PUT (update) individual application PFD
if [ -n "$TRANS_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" -X PUT "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions/${TRANS_ID}/applications/app1" \
      -H "Content-Type: application/json" \
      -d '{
        "externalAppId": "app1",
        "pfds": {
          "pfd1_updated": {
            "pfdId": "pfd1_updated",
            "flowDescriptions": ["permit out ip from 10.1.0.0/24 to any"],
            "urls": ["https://newexample.com/video"]
          }
        }
      }')
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "PUT Individual App PFD (update app1)" "200" "$HTTP_CODE" "$BODY"
fi

# Test 4.7: PATCH individual application PFD (add a new PFD)
if [ -n "$TRANS_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" -X PATCH "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions/${TRANS_ID}/applications/app1" \
      -H "Content-Type: application/json" \
      -d '{
        "externalAppId": "app1",
        "pfds": {
          "pfd_extra": {
            "pfdId": "pfd_extra",
            "urls": ["https://extra.example.com"]
          }
        }
      }')
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "PATCH Individual App PFD (add to app1)" "200" "$HTTP_CODE" "$BODY"
fi

# Test 4.8: DELETE individual application PFD
if [ -n "$TRANS_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" -X DELETE "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions/${TRANS_ID}/applications/app2")
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "DELETE Individual App PFD (app2)" "204" "$HTTP_CODE" "$BODY"
fi

# Test 4.9: Verify SBI PFD view after changes
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/nnef-pfdmanagement/v1/applications?application-ids=app1")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET SBI PFD for app1" "200" "$HTTP_CODE" "$BODY"

# Test 4.10: GET individual app PFD via SBI
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/nnef-pfdmanagement/v1/applications/app1")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET SBI Individual App PFD (app1)" "200" "$HTTP_CODE" "$BODY"

# Test 4.11: DELETE entire PFD transaction
if [ -n "$TRANS_ID" ]; then
    RESPONSE=$(curl -s -w "\n%{http_code}" -X DELETE "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions/${TRANS_ID}")
    HTTP_CODE=$(echo "$RESPONSE" | tail -1)
    BODY=$(echo "$RESPONSE" | sed '$d')
    check_result "DELETE Individual PFD Transaction" "204" "$HTTP_CODE" "$BODY"
fi

# ============================================================
# 5. Error / Edge Case Tests
# ============================================================
echo ""
echo -e "${YELLOW}--- 5. Error & Edge Case Tests ---${NC}"

# Test 5.1: GET non-existent AF
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-pfd-management/v1/nonexistentAF/transactions")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET PFD for non-existent AF" "404" "$HTTP_CODE" "$BODY"

# Test 5.2: POST PFD with missing pfdDatas
RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions" \
  -H "Content-Type: application/json" \
  -d '{"pfdDatas": {}}')
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "POST PFD with empty pfdDatas" "404" "$HTTP_CODE" "$BODY"

# Test 5.3: POST TI with missing required fields
RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions" \
  -H "Content-Type: application/json" \
  -d '{"afAppId": "testApp"}')
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "POST TI with missing UE identifier" "400" "$HTTP_CODE" "$BODY"

# Test 5.4: POST TI with missing traffic routes
RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions" \
  -H "Content-Type: application/json" \
  -d '{
    "afAppId": "testApp",
    "anyUeInd": true
  }')
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "POST TI with missing traffic routes" "400" "$HTTP_CODE" "$BODY"

# Test 5.5: GET non-existent subscription
RESPONSE=$(curl -s -w "\n%{http_code}" "${NEF_BASE}/3gpp-traffic-influence/v1/${AF_ID}/subscriptions/999")
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "GET non-existent TI subscription" "404" "$HTTP_CODE" "$BODY"

# Test 5.6: POST PFD with missing PFD IDs
RESPONSE=$(curl -s -w "\n%{http_code}" -X POST "${NEF_BASE}/3gpp-pfd-management/v1/${AF_ID}/transactions" \
  -H "Content-Type: application/json" \
  -d '{
    "pfdDatas": {
      "badApp": {
        "externalAppId": "badApp",
        "pfds": {
          "noid": {
            "pfdId": "",
            "flowDescriptions": ["permit out ip from 10.0.0.0/24 to any"]
          }
        }
      }
    }
  }')
HTTP_CODE=$(echo "$RESPONSE" | tail -1)
BODY=$(echo "$RESPONSE" | sed '$d')
check_result "POST PFD with empty pfdId" "404" "$HTTP_CODE" "$BODY"

# ============================================================
# Summary
# ============================================================
echo ""
echo -e "${CYAN}============================================${NC}"
TOTAL=$((PASS + FAIL))
echo -e "${CYAN}   Test Results: ${PASS}/${TOTAL} passed${NC}"
if [ $FAIL -eq 0 ]; then
    echo -e "${GREEN}   All tests PASSED! ✓${NC}"
else
    echo -e "${RED}   ${FAIL} test(s) FAILED ✗${NC}"
fi
echo -e "${CYAN}============================================${NC}"

exit $FAIL
