package ble

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"

	"aiden-agent/internal/logging"
)

var errPairingModeState = errors.New("BLE pairing mode update failed")

type bondedDevice struct {
	path       dbus.ObjectPath
	properties map[string]dbus.Variant
}

func bondedDevices(objects managedObjects) []bondedDevice {
	devices := make([]bondedDevice, 0)
	for path, interfaces := range objects {
		properties, ok := interfaces[blueZDeviceInterface]
		if !ok || !variantBool(properties, "Paired") {
			continue
		}
		devices = append(devices, bondedDevice{path: path, properties: properties})
	}
	sort.Slice(devices, func(i, j int) bool {
		iConnected := variantBool(devices[i].properties, "Connected")
		jConnected := variantBool(devices[j].properties, "Connected")
		if iConnected != jConnected {
			return iConnected
		}
		iTrusted := variantBool(devices[i].properties, "Trusted")
		jTrusted := variantBool(devices[j].properties, "Trusted")
		if iTrusted != jTrusted {
			return iTrusted
		}
		return string(devices[i].path) < string(devices[j].path)
	})
	return devices
}

func selectBondedDevice(objects managedObjects) (dbus.ObjectPath, int) {
	devices := bondedDevices(objects)
	if len(devices) == 0 {
		return "", 0
	}
	return devices[0].path, len(devices)
}

func (b *blueZBackend) refreshTrustedDevice(objects managedObjects) (dbus.ObjectPath, int, error) {
	devices := bondedDevices(objects)
	b.stateMu.Lock()
	previous := b.trustedDevice
	selected := dbus.ObjectPath("")
	for _, device := range devices {
		if variantBool(device.properties, "Connected") {
			selected = device.path
			break
		}
	}
	for _, device := range devices {
		if !selected.IsValid() && device.path == previous {
			selected = previous
			break
		}
	}
	if !selected.IsValid() && len(devices) > 0 {
		selected = devices[0].path
	}
	b.trustedDevice = selected
	b.stateMu.Unlock()

	if selected.IsValid() {
		if !variantBool(objects[selected][blueZDeviceInterface], "Trusted") {
			if err := b.markDeviceTrusted(selected); err != nil {
				b.service.status.update(func(status *RuntimeStatus) { status.LastError = err.Error() })
			}
		}
	}
	return selected, len(devices), nil
}

func (b *blueZBackend) StartPairing() error {
	b.wakeMu.Lock()
	closed := b.closed
	b.wakeMu.Unlock()
	if closed {
		return ErrBluetoothUnavailable
	}
	b.setConnectionEnabled(true)
	if err := b.setAdvertising(true); err != nil {
		b.setConnectionEnabled(false)
		return err
	}

	// A bond is only a transport cache. An explicit Connect action must always
	// reopen the window so the current phone can reconnect, finish an incomplete
	// authentication, or create a new bond after either side forgot the old one.
	return b.beginPairingWindow(time.Now())
}

func connectedDevicePaths(objects managedObjects) []dbus.ObjectPath {
	paths := make([]dbus.ObjectPath, 0)
	for path, interfaces := range objects {
		properties, ok := interfaces[blueZDeviceInterface]
		if !ok || !variantBool(properties, "Connected") {
			continue
		}
		paths = append(paths, path)
	}
	sort.Slice(paths, func(i, j int) bool { return string(paths[i]) < string(paths[j]) })
	return paths
}

func (b *blueZBackend) disconnectConnectedDevices(objects managedObjects) error {
	var result error
	for _, path := range connectedDevicePaths(objects) {
		err := callWithTimeout(
			b.conn.Object(BlueZBusName, path),
			blueZDeviceInterface+".Disconnect",
		).Err
		if err != nil && !isDBusErrorNamed(err, "org.bluez.Error.NotConnected") {
			result = errors.Join(result, fmt.Errorf("disconnect Bluetooth device %s: %w", path, err))
		}
	}
	return result
}

func (b *blueZBackend) Disconnect() error {
	b.wakeMu.Lock()
	closed := b.closed
	b.wakeMu.Unlock()
	if closed || b.conn == nil || !b.adapter.IsValid() {
		return ErrBluetoothUnavailable
	}
	logging.Infof("ble_service", "pairing", "BLE user disconnect requested")
	b.setConnectionEnabled(false)
	var result error
	result = errors.Join(result, b.closePairingWindow())
	result = errors.Join(result, b.setAdvertising(false))

	objects, err := b.getManagedObjects()
	if err != nil {
		result = errors.Join(result, err)
	} else {
		result = errors.Join(result, b.disconnectConnectedDevices(objects))
	}

	b.clearANCS("Bluetooth disconnected by user")
	b.service.consumer.ResetConnection("Bluetooth disconnected by user")
	b.service.status.update(func(status *RuntimeStatus) {
		status.PairingOpen = false
		status.PairingDeadline = ""
		status.Advertising = false
		status.ConnectedDevicePath = ""
		status.ConnectedDeviceName = ""
		status.ConnectedDeviceAddr = ""
		status.WakeSubscriber = false
		status.Connected = false
		status.ServicesResolved = false
		status.ANCSSubscribed = false
		status.LastError = ""
	})
	b.requestRescan()
	return result
}

func (b *blueZBackend) ForgetPairing() (int, error) {
	b.wakeMu.Lock()
	closed := b.closed
	b.wakeMu.Unlock()
	if closed || b.conn == nil || !b.adapter.IsValid() {
		return 0, ErrBluetoothUnavailable
	}
	if err := b.closePairingWindow(); err != nil {
		return 0, err
	}

	objects, err := b.getManagedObjects()
	if err != nil {
		return 0, err
	}
	devices := bondedDevices(objects)
	removed := 0
	for _, device := range devices {
		if err := callWithTimeout(
			b.conn.Object(BlueZBusName, b.adapter),
			blueZAdapterInterface+".RemoveDevice",
			device.path,
		).Err; err != nil {
			logging.Warnf("ble_service", "pairing", "BLE bond removal failed device=%s removed=%d: %v", device.path, removed, err)
			b.requestRescan()
			return removed, fmt.Errorf("remove Bluetooth bond %s: %w", device.path, err)
		}
		removed++
	}
	logging.Infof("ble_service", "pairing", "BLE board bonds removed count=%d", removed)

	b.stateMu.Lock()
	b.trustedDevice = ""
	b.stateMu.Unlock()
	b.clearANCS("Bluetooth pairing removed")
	b.service.consumer.ResetConnection("Bluetooth pairing removed")
	b.service.status.update(func(status *RuntimeStatus) {
		status.BondedDeviceCount = 0
		status.TrustedDevicePath = ""
		status.TrustedDeviceName = ""
		status.TrustedDeviceAddr = ""
		status.ConnectedDevicePath = ""
		status.ConnectedDeviceName = ""
		status.ConnectedDeviceAddr = ""
		status.WakeSubscriber = false
		status.Connected = false
		status.Paired = false
		status.ServicesResolved = false
		status.ANCSSubscribed = false
	})
	b.requestRescan()
	return removed, nil
}

func (b *blueZBackend) markDeviceTrusted(device dbus.ObjectPath) error {
	if !device.IsValid() {
		return nil
	}
	if err := callWithTimeout(
		b.conn.Object(BlueZBusName, device),
		dbusPropertiesInterface+".Set",
		blueZDeviceInterface,
		"Trusted",
		dbus.MakeVariant(true),
	).Err; err != nil {
		return fmt.Errorf("trust paired Bluetooth device: %w", err)
	}
	return nil
}

func (b *blueZBackend) updateDeviceStatus(
	trusted dbus.ObjectPath,
	properties map[string]dbus.Variant,
	bondedCount int,
) {
	b.stateMu.Lock()
	pairingOpen := b.pairingOpen
	pairingDeadline := b.pairingDeadline
	connectionEnabled := b.connectionEnabled
	b.stateMu.Unlock()
	connected := connectionEnabled && trusted.IsValid() && variantBool(properties, "Connected")
	if previous := b.service.Status(); previous.Connected != connected ||
		previous.BondedDeviceCount != bondedCount {
		logging.Infof(
			"ble_service", "pairing",
			"BLE device state connected=%t paired=%t services_resolved=%t bonded=%d pairing_open=%t device=%s",
			connected, variantBool(properties, "Paired"), variantBool(properties, "ServicesResolved"),
			bondedCount, pairingOpen, trusted,
		)
	}
	if !connected {
		// BlueZ normally invokes StopNotify when the central disconnects, but an
		// abrupt link loss or bluetoothd restart can omit that callback. Clear the
		// exported characteristic state here as well so status and Wake delivery
		// never claim that a disconnected iPhone is still subscribed.
		b.wakeMu.Lock()
		if b.wakeProps != nil {
			if notifying, _ := b.wakeProps.GetMust(blueZGattCharInterface, "Notifying").(bool); notifying {
				b.wakeProps.SetMust(blueZGattCharInterface, "Notifying", false)
			}
		}
		b.wakeMu.Unlock()
	}

	b.service.status.update(func(status *RuntimeStatus) {
		status.BondedDeviceCount = bondedCount
		status.PairingOpen = pairingOpen
		status.PairingDeadline = formatDeadline(pairingDeadline)
		status.TrustedDevicePath = ""
		status.TrustedDeviceName = ""
		status.TrustedDeviceAddr = ""
		status.ConnectedDevicePath = ""
		status.ConnectedDeviceName = ""
		status.ConnectedDeviceAddr = ""
		status.Connected = false
		status.Paired = trusted.IsValid()
		status.ServicesResolved = false
		if !connected {
			status.WakeSubscriber = false
			status.ANCSSubscribed = false
		}
		if !trusted.IsValid() {
			return
		}
		name := firstNonEmpty(variantString(properties, "Name"), variantString(properties, "Alias"))
		address := variantString(properties, "Address")
		status.TrustedDevicePath = string(trusted)
		status.TrustedDeviceName = name
		status.TrustedDeviceAddr = address
		status.Connected = connected
		status.ServicesResolved = status.Connected && variantBool(properties, "ServicesResolved")
		if status.Connected {
			status.ConnectedDevicePath = string(trusted)
			status.ConnectedDeviceName = name
			status.ConnectedDeviceAddr = address
		}
	})
}

func (b *blueZBackend) setConnectionEnabled(enabled bool) {
	b.stateMu.Lock()
	b.connectionEnabled = enabled
	b.stateMu.Unlock()
}

func (b *blueZBackend) connectionsEnabled() bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.connectionEnabled
}

func (b *blueZBackend) beginPairingWindow(now time.Time) error {
	if b.pairingWindow <= 0 {
		return nil
	}
	b.pairingModeMu.Lock()
	defer b.pairingModeMu.Unlock()

	b.stateMu.Lock()
	b.pairingOpen = true
	b.pairingOpenedAt = now
	b.pairingDeadline = now.Add(b.pairingWindow)
	b.pairingModeDirty = true
	deadline := b.pairingDeadline
	b.stateMu.Unlock()

	if err := b.setPairingMode(true); err != nil {
		// Reapply the complete closed adapter state. Advertisement registration
		// is intentionally independent from pairing-window transitions.
		cleanupErr := b.setPairingMode(false)
		b.stateMu.Lock()
		b.pairingOpen = false
		b.pairingOpenedAt = time.Time{}
		b.pairingDeadline = time.Time{}
		b.pairingModeDirty = cleanupErr != nil
		b.stateMu.Unlock()
		resultErr := fmt.Errorf("%w: %v", errPairingModeState, err)
		logging.Warnf("ble_service", "pairing", "BLE pairing window open failed: %v", resultErr)
		b.service.status.update(func(status *RuntimeStatus) {
			status.PairingOpen = false
			status.PairingDeadline = ""
			status.LastError = resultErr.Error()
		})
		return resultErr
	}
	b.stateMu.Lock()
	b.pairingModeDirty = false
	b.stateMu.Unlock()
	logging.Infof("ble_service", "pairing", "BLE pairing window opened deadline=%s", formatDeadline(deadline))
	b.service.status.update(func(status *RuntimeStatus) {
		status.PairingOpen = true
		status.PairingDeadline = formatDeadline(deadline)
		status.LastError = ""
	})
	return nil
}

func (b *blueZBackend) closePairingWindow() error {
	b.pairingModeMu.Lock()
	defer b.pairingModeMu.Unlock()
	return b.closePairingWindowLocked()
}

// closePairingWindowOpenedAt closes the window only if it is still the one
// opened at openedAt. The check and the close share one pairingModeMu
// acquisition so a concurrent explicit Connect cannot open a newer window in
// between and have it closed by a completion that belonged to the old one.
func (b *blueZBackend) closePairingWindowOpenedAt(openedAt time.Time) (bool, error) {
	b.pairingModeMu.Lock()
	defer b.pairingModeMu.Unlock()
	b.stateMu.Lock()
	sameWindow := b.pairingOpen && b.pairingOpenedAt.Equal(openedAt)
	b.stateMu.Unlock()
	if !sameWindow {
		return false, nil
	}
	return true, b.closePairingWindowLocked()
}

// closePairingWindowLocked requires pairingModeMu.
func (b *blueZBackend) closePairingWindowLocked() error {
	b.stateMu.Lock()
	wasOpen := b.pairingOpen
	wasDirty := b.pairingModeDirty
	b.pairingOpen = false
	b.pairingOpenedAt = time.Time{}
	b.pairingDeadline = time.Time{}
	b.pairingModeDirty = wasOpen || wasDirty
	b.stateMu.Unlock()

	if !wasOpen && !wasDirty {
		return nil
	}
	// Always reapply the closed state. This also recovers from a previous
	// partial D-Bus failure that may have left one adapter property enabled.
	if err := b.setPairingMode(false); err != nil {
		resultErr := fmt.Errorf("%w: %v", errPairingModeState, err)
		b.service.status.update(func(status *RuntimeStatus) {
			status.PairingOpen = false
			status.PairingDeadline = ""
			status.LastError = resultErr.Error()
		})
		return resultErr
	}
	b.stateMu.Lock()
	b.pairingModeDirty = false
	b.stateMu.Unlock()
	b.service.status.update(func(status *RuntimeStatus) {
		status.PairingOpen = false
		status.PairingDeadline = ""
		status.LastError = ""
	})
	return nil
}

func (b *blueZBackend) expirePairingWindow(now time.Time) error {
	b.stateMu.Lock()
	expired := b.pairingOpen && !b.pairingDeadline.IsZero() && !now.Before(b.pairingDeadline)
	b.stateMu.Unlock()
	if expired {
		logging.Infof("ble_service", "pairing", "BLE pairing window closed reason=deadline")
		return b.closePairingWindow()
	}
	return nil
}

func (b *blueZBackend) reconcilePairingMode() error {
	b.pairingModeMu.Lock()
	defer b.pairingModeMu.Unlock()

	b.stateMu.Lock()
	open := b.pairingOpen
	b.stateMu.Unlock()
	objects, err := b.getManagedObjects()
	if err != nil {
		return err
	}
	properties := objects[b.adapter][blueZAdapterInterface]
	if pairingModeMatches(properties, open) {
		return nil
	}
	return b.setPairingMode(open)
}

func pairingModeMatches(properties map[string]dbus.Variant, open bool) bool {
	return variantBool(properties, "Pairable") == open &&
		variantBool(properties, "Discoverable") == open
}

func (b *blueZBackend) setPairingMode(open bool) error {
	if b.conn == nil || !b.adapter.IsValid() {
		return ErrBluetoothUnavailable
	}
	return applyAdapterPairingMode(open, func(name string, value bool) error {
		return callWithTimeout(
			b.conn.Object(BlueZBusName, b.adapter),
			dbusPropertiesInterface+".Set",
			blueZAdapterInterface,
			name,
			dbus.MakeVariant(value),
		).Err
	})
}

func applyAdapterPairingMode(open bool, setProperty func(name string, value bool) error) error {
	for _, name := range []string{"Pairable", "Discoverable"} {
		if err := setProperty(name, open); err != nil {
			return fmt.Errorf("set adapter %s: %w", name, err)
		}
	}
	return nil
}

func advertisedServiceUUIDs() []string {
	return []string{WakeServiceUUID}
}

func advertisedManufacturerData(boardIdentity []byte) map[uint16]dbus.Variant {
	identity := append([]byte(nil), boardIdentity...)
	return map[uint16]dbus.Variant{
		AidenManufacturerID: dbus.MakeVariant(identity),
	}
}

func (b *blueZBackend) deviceAllowed(device dbus.ObjectPath) bool {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if b.pairingOpen {
		return true
	}
	if b.trustedDevice.IsValid() {
		return device == b.trustedDevice
	}
	return false
}

func stableDeviceName(baseName, adapterAddress string) string {
	const maxLocalNameBytes = 29
	baseName = strings.TrimSpace(baseName)
	if baseName == "" {
		baseName = "Aiden"
	}
	compactAddress := strings.NewReplacer(":", "", "-", "").Replace(strings.ToUpper(adapterAddress))
	if len(compactAddress) < 4 {
		return truncateUTF8(baseName, maxLocalNameBytes)
	}
	suffix := compactAddress[len(compactAddress)-4:]
	if strings.HasSuffix(strings.ToUpper(baseName), "-"+suffix) {
		baseName = baseName[:len(baseName)-len(suffix)-1]
	}
	maxBaseBytes := maxLocalNameBytes - len(suffix) - 1
	if len(baseName) > maxBaseBytes {
		baseName = truncateUTF8(baseName, maxBaseBytes)
	}
	return baseName + "-" + suffix
}

func boardIdentityFromAdapterAddress(adapterAddress string) (string, []byte, error) {
	compactAddress := strings.NewReplacer(":", "", "-", "").Replace(
		strings.TrimSpace(adapterAddress),
	)
	identity, err := hex.DecodeString(compactAddress)
	if err != nil || len(identity) != 6 {
		return "", nil, fmt.Errorf("invalid Bluetooth adapter address %q", adapterAddress)
	}
	return strings.ToLower(hex.EncodeToString(identity)), identity, nil
}

func truncateUTF8(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func formatDeadline(deadline time.Time) string {
	if deadline.IsZero() {
		return ""
	}
	return deadline.UTC().Format(time.RFC3339Nano)
}
