#!/usr/bin/env bash
# Spins up two containers on the same Docker bridge network (stand-ins for
# "two machines on the same Wi-Fi"), creates a pool on device-a and tries to
# join it from device-b, then reports whether discovery/join worked.
set -uo pipefail
cd "$(dirname "$0")"

echo "=== building & starting containers ==="
docker compose up -d --build

echo "=== starting pool on device-a ==="
docker compose exec -T device-a sh -c 'nohup wifi-cursor create > /tmp/a.log 2>&1 & sleep 2; cat /tmp/a.log'

POOL_ID=$(docker compose exec -T device-a sh -c "grep -oE 'Пул создан: [A-Z0-9]+' /tmp/a.log | awk '{print \$3}'" | tr -d '\r')
echo "POOL_ID=$POOL_ID"

if [ -z "$POOL_ID" ]; then
  echo "FAIL: could not read pool ID from device-a"
  docker compose logs
  docker compose down
  exit 1
fi

echo "=== joining from device-b ==="
docker compose exec -T device-b sh -c "timeout 8 wifi-cursor join $POOL_ID" > /tmp/wifi-cursor-b.log 2>&1
cat /tmp/wifi-cursor-b.log

if grep -q "Подключено к пулу" /tmp/wifi-cursor-b.log; then
  echo "RESULT: SUCCESS - join worked"
else
  echo "RESULT: FAILURE - join did not succeed"
fi

echo "=== cleanup ==="
docker compose down
