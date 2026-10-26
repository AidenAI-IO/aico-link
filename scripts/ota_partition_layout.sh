#!/usr/bin/env bash

AICO_OTA_PARTITION_NAME="${AICO_OTA_PARTITION_NAME:-ota}"
AICO_OTA_MOUNT_POINT="${AICO_OTA_MOUNT_POINT:-/userdata/ota}"
AICO_OTA_DEVICE_PATH="${AICO_OTA_DEVICE_PATH:-/dev/disk/by-partlabel/ota}"
AICO_OTA_FILESYSTEM="${AICO_OTA_FILESYSTEM:-ext4}"
AICO_OTA_DOWNLOAD_SAFETY_MARGIN_MIB="${AICO_OTA_DOWNLOAD_SAFETY_MARGIN_MIB:-16}"
AICO_OTA_FILESYSTEM_OVERHEAD_MIB="${AICO_OTA_FILESYSTEM_OVERHEAD_MIB:-30}"
AICO_OTA_LAYOUT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
AICO_OTA_BOARD_CONFIG_PATH="${AICO_OTA_BOARD_CONFIG_PATH:-$AICO_OTA_LAYOUT_ROOT/scripts/debian-system/BoardConfig-EMMC-Debian13-RV1106_Luckfox_Pico_Zero-IPC.mk}"

aico_ota_partition_size_mib() {
  local partition_env definition size_spec partition_name value

  if [ ! -f "$AICO_OTA_BOARD_CONFIG_PATH" ]; then
    echo "ota_partition_layout.sh: missing board config: $AICO_OTA_BOARD_CONFIG_PATH" >&2
    return 1
  fi
  partition_env="$({
    unset RK_PARTITION_CMD_IN_ENV
    source "$AICO_OTA_BOARD_CONFIG_PATH"
    printf '%s' "${RK_PARTITION_CMD_IN_ENV:-}"
  })" || return 1

  while IFS= read -r definition; do
    partition_name="${definition#*(}"
    partition_name="${partition_name%)}"
    [ "$partition_name" = "$AICO_OTA_PARTITION_NAME" ] || continue

    size_spec="${definition%%(*}"
    size_spec="${size_spec%%@*}"
    value="${size_spec%?}"
    case "$size_spec" in
      *K|*k)
        [[ "$value" =~ ^[0-9]+$ ]] && [ $((value % 1024)) -eq 0 ] || break
        echo $((value / 1024))
        return 0
        ;;
      *M|*m)
        [[ "$value" =~ ^[0-9]+$ ]] || break
        echo "$value"
        return 0
        ;;
      *G|*g)
        [[ "$value" =~ ^[0-9]+$ ]] || break
        echo $((value * 1024))
        return 0
        ;;
    esac
    break
  done < <(printf '%s\n' "$partition_env" | tr ',' '\n')

  echo "ota_partition_layout.sh: valid ${AICO_OTA_PARTITION_NAME} partition not found in $AICO_OTA_BOARD_CONFIG_PATH" >&2
  return 1
}

aico_ota_partition_size_bytes() {
  local partition_mib
  partition_mib="$(aico_ota_partition_size_mib)" || return 1
  echo $((partition_mib * 1024 * 1024))
}

aico_ota_download_safety_margin_bytes() {
  echo $((AICO_OTA_DOWNLOAD_SAFETY_MARGIN_MIB * 1024 * 1024))
}

aico_ota_manifest_max_download_bytes() {
  local partition_mib usable_mib
  partition_mib="$(aico_ota_partition_size_mib)" || return 1
  usable_mib=$((partition_mib - AICO_OTA_DOWNLOAD_SAFETY_MARGIN_MIB - AICO_OTA_FILESYSTEM_OVERHEAD_MIB))
  if [ "$usable_mib" -le 0 ]; then
    echo "ota_partition_layout.sh: OTA partition has no usable download capacity" >&2
    return 1
  fi
  echo $((usable_mib * 1024 * 1024))
}
