#!/bin/bash

if [ "$(cat /proc/device-tree/model)" == "Luckfox Pico Ultra W" ] ||
	[ "$(cat /proc/device-tree/model)" == "Luckfox Pico 86Panel W" ] ||
	[ "$(cat /proc/device-tree/model)" == "Luckfox Pico Pi W" ]; then
	systemctl stop wpa_supplicant

	if [ "$1" = "stop" ]; then
		pkill -f "udhcpc -i wlan0"
		exit 1
	fi

	if [ -d /oem/usr/ko ]; then
		cd /oem/usr/ko
		if [ -z "$(ifconfig | grep "wlan0")" ]; then
			insmod cfg80211.ko
			insmod libarc4.ko
			insmod ctr.ko
			insmod ccm.ko
			insmod aes_generic.ko
			insmod aic8800_bsp.ko
			sleep 0.2
			insmod aic8800_fdrv.ko
			sleep 2
			insmod aic8800_btlpm.ko
		else
			# wait systemctl
			sleep 0.5
		fi
	else
		echo "Missing ko files!"
	fi

	if [ -d /var/run/wpa_supplicant ]; then
		rm /var/run/wpa_supplicant/ -rf
	fi

	WPA_CONF=/etc/wpa_supplicant.conf
	if [ -f /userdata/wpa_supplicant.conf ]; then
		WPA_CONF=/userdata/wpa_supplicant.conf
	elif [ -f /data/wpa_supplicant.conf ]; then
		WPA_CONF=/data/wpa_supplicant.conf
	elif [ -f /data/cfg/wpa_supplicant.conf ]; then
		WPA_CONF=/data/cfg/wpa_supplicant.conf
	fi

	if [ -f "$WPA_CONF" ] && [ -n "$(ifconfig | grep "wlan0")" ]; then
		wpa_supplicant -B -i wlan0 -c "$WPA_CONF" >/dev/null
		chmod a+x /usr/share/udhcpc/default.script
	fi

else
	echo "This Luckfox Pico model don't support WIFI!"
fi
