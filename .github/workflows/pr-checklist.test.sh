#!/usr/bin/env bash
# pr-checklist parser tests
# Covers the logic in .github/workflows/pr-checklist.yml
# Run locally: bash .github/workflows/pr-checklist.test.sh

set -e

PASS=0
FAIL=0

check_pr() {
  local label="$1"
  local body="$2"
  local expect_fail="$3"

  HAS_HUMAN_VERIFIED_LABEL="$label"

  errors=()

  if [ -n "$body" ] && echo "$body" | grep -qE '^- \[ \]'; then
    errors+=("unchecked")
  fi

  if [ "$HAS_HUMAN_VERIFIED_LABEL" != "true" ]; then
    errors+=("no-label")
  fi

  failed=0
  if [ ${#errors[@]} -gt 0 ]; then
    failed=1
  fi

  # Normalize expect_fail to 0/1
  if [ "$expect_fail" = "true" ]; then
    expect_fail=1
  else
    expect_fail=0
  fi

  if [ "$failed" -eq "$expect_fail" ]; then
    echo "  PASS"
    PASS=$((PASS+1))
  else
    echo "  FAIL (expected fail=$expect_fail, got fail=$failed, errors=${errors[*]})"
    FAIL=$((FAIL+1))
  fi
}

echo "=== pr-checklist parser tests ==="

# Test 1: empty body + no label → fail (label required)
echo "Test 1: empty body + no label → fail"
check_pr "false" "" "true"

# Test 2: all boxes checked + no label → fail (no label)
echo "Test 2: all boxes checked + no label → fail"
check_pr "false" $'- [x] item1\n- [x] item2' "true"

# Test 3: all boxes checked + human-verified label → pass
echo "Test 3: all boxes checked + human-verified label → pass"
check_pr "true" $'- [x] item1\n- [x] item2' "false"

# Test 4: unchecked box + human-verified label → fail (unchecked box)
echo "Test 4: unchecked box + human-verified label → fail"
check_pr "true" $'- [ ] item1\n- [x] item2' "true"

# Test 5: unchecked box + no label → fail (both signals missing)
echo "Test 5: unchecked box + no label → fail"
check_pr "false" $'- [ ] item1\n- [x] item2' "true"

# Test 6: all boxes checked + no label → fail (no label only)
echo "Test 6: all boxes checked + no label → fail (no label only)"
check_pr "false" $'- [x] item1\n- [x] item2' "true"

# Test 7: body with no checkbox items + no label → fail (label always required)
echo "Test 7: body with no checkbox items + no label → fail"
check_pr "false" $"## Summary\n\nSome description" "true"

echo ""
echo "=== Results: PASS=$PASS FAIL=$FAIL ==="
[ "$FAIL" -eq 0 ]
