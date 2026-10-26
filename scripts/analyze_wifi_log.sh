#!/bin/bash

echo "=== WiFi Driver Log Analysis ==="
echo ""
echo "Total lines: $(wc -l < wifi_driver.log)"
echo "Total boot attempts: $(grep -c "stage=begin" wifi_driver.log)"
echo "Successful BSP loads: $(grep "stage=bsp.*result=success" wifi_driver.log | wc -l)"
echo "Failed BSP loads: $(grep "stage=bsp.*result=failed" wifi_driver.log | wc -l)"
echo ""

echo "=== Unique MMC Card IDs (每次启动可能不同) ==="
grep "aiden-wifi-prepare" wifi_driver.log | grep -o "card=mmc2:[0-9a-f]*" | sort -u | head -20
echo ""

echo "=== First successful attempt (if any) ==="
grep -B2 "stage=fdrv.*result=success" wifi_driver.log | head -10
echo ""

echo "=== Last 3 attempts ==="
grep -n "stage=begin" wifi_driver.log | tail -3
