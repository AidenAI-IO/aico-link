#!/bin/bash
BACKUP_MIRROR_SITE="http://sources.buildroot.net"
DEFAULT_PRIMARY_MIRROR_SITE="https://mirrors.nju.edu.cn/buildroot/"
PRIMARY_MIRROR_SITE="${AIDEN_BUILDROOT_MIRROR_URL:-$DEFAULT_PRIMARY_MIRROR_SITE}"
echo "Primary mirror is $PRIMARY_MIRROR_SITE"

PRIMARY_MIRROR_STRING='BR2_PRIMARY_SITE='
BACKUP_MIRROR_STRING='BR2_BACKUP_SITE='

CONFIG_PATH="$1"
CONFIG_TMP="${CONFIG_PATH}.tmp.$$"
while IFS= read -r line || [ -n "$line" ]; do
	case "$line" in
		"$PRIMARY_MIRROR_STRING"*)
			printf 'BR2_PRIMARY_SITE="%s"\n' "$PRIMARY_MIRROR_SITE"
			;;
		"$BACKUP_MIRROR_STRING"*)
			printf 'BR2_BACKUP_SITE="%s"\n' "$BACKUP_MIRROR_SITE"
			;;
		*)
			printf '%s\n' "$line"
			;;
	esac
done < "$CONFIG_PATH" > "$CONFIG_TMP"
mv "$CONFIG_TMP" "$CONFIG_PATH"
