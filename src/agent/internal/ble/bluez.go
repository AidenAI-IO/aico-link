package ble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"

	"aiden-agent/internal/logging"
)

const (
	blueZDBusCallTimeout    = 5 * time.Second
	blueZCleanupCallTimeout = time.Second
	wakeStopGracePeriod     = 2 * time.Second

	dbusBusName                  = "org.freedesktop.DBus"
	dbusBusInterface             = "org.freedesktop.DBus"
	dbusPropertiesInterface      = "org.freedesktop.DBus.Properties"
	dbusObjectManagerInterface   = "org.freedesktop.DBus.ObjectManager"
	blueZAdapterInterface        = "org.bluez.Adapter1"
	blueZDeviceInterface         = "org.bluez.Device1"
	blueZGattManagerInterface    = "org.bluez.GattManager1"
	blueZGattServiceInterface    = "org.bluez.GattService1"
	blueZGattCharInterface       = "org.bluez.GattCharacteristic1"
	blueZGattDescriptorInterface = "org.bluez.GattDescriptor1"
	blueZAdvManagerInterface     = "org.bluez.LEAdvertisingManager1"
	blueZAdvertisementInterface  = "org.bluez.LEAdvertisement1"
	blueZAgentManagerInterface   = "org.bluez.AgentManager1"
	blueZAgentInterface          = "org.bluez.Agent1"

	applicationPath   = dbus.ObjectPath("/com/aiden/ble")
	wakeServicePath   = dbus.ObjectPath("/com/aiden/ble/service0")
	wakeCharPath      = dbus.ObjectPath("/com/aiden/ble/service0/char0")
	advertisementPath = dbus.ObjectPath("/com/aiden/ble/advertisement0")
	agentPath         = dbus.ObjectPath("/com/aiden/ble/agent0")
	blueZRootPath     = dbus.ObjectPath("/")
)

type managedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

type ancsPaths struct {
	device             dbus.ObjectPath
	notificationSource dbus.ObjectPath
	controlPoint       dbus.ObjectPath
	dataSource         dbus.ObjectPath
}

type exportedGattObject struct {
	path          dbus.ObjectPath
	interfaceName string
	properties    *prop.Properties
}

func (p ancsPaths) complete() bool {
	return p.device.IsValid() && p.notificationSource.IsValid() &&
		p.controlPoint.IsValid() && p.dataSource.IsValid()
}

type blueZBackend struct {
	service        *Service
	deviceName     string
	boardIdentity  []byte
	pairingWindow  time.Duration
	conn           *dbus.Conn
	adapter        dbus.ObjectPath
	signals        chan *dbus.Signal
	blueZOwner     string
	rescanRequests chan struct{}
	fatalErrors    chan error

	wakeProps   *prop.Properties
	advProps    *prop.Properties
	wakeObject  *wakeCharacteristic
	gattObjects []exportedGattObject

	ancsMu              sync.Mutex
	ancs                ancsPaths
	advertisementMu     sync.Mutex
	advertising         bool
	wakeMu              sync.Mutex
	lastWakeNotifyStart time.Time
	wakeSubscribedAt    time.Time
	wakeAuthenticatedAt time.Time
	closed              bool
	pairingModeMu       sync.Mutex
	stateMu             sync.Mutex
	trustedDevice       dbus.ObjectPath
	connectionEnabled   bool
	pairingOpen         bool
	pairingOpenedAt     time.Time
	pairingDeadline     time.Time
	pairingModeDirty    bool
}

func newBlueZBackend(service *Service, deviceName string, pairingWindow time.Duration) *blueZBackend {
	if strings.TrimSpace(deviceName) == "" {
		deviceName = "Aiden"
	}
	if pairingWindow < 0 {
		pairingWindow = 0
	}
	return &blueZBackend{
		service:           service,
		deviceName:        deviceName,
		pairingWindow:     pairingWindow,
		rescanRequests:    make(chan struct{}, 1),
		fatalErrors:       make(chan error, 1),
		connectionEnabled: true,
	}
}

func (b *blueZBackend) start() error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connect system D-Bus: %w", err)
	}
	b.conn = conn
	b.signals = make(chan *dbus.Signal, 64)
	b.conn.Signal(b.signals)
	if err := callWithTimeout(b.conn.BusObject(), "org.freedesktop.DBus.GetNameOwner", BlueZBusName).
		Store(&b.blueZOwner); err != nil {
		return fmt.Errorf("resolve BlueZ D-Bus owner: %w", err)
	}

	if err := b.addSignalMatches(); err != nil {
		return fmt.Errorf("subscribe BlueZ signals: %w", err)
	}

	objects, err := b.getManagedObjects()
	if err != nil {
		return err
	}
	b.adapter = findAdapter(objects)
	if !b.adapter.IsValid() {
		return errors.New("BlueZ adapter hci0 is unavailable")
	}
	adapterProps := objects[b.adapter][blueZAdapterInterface]
	adapterAddress := variantString(adapterProps, "Address")
	identityText, identityBytes, err := boardIdentityFromAdapterAddress(adapterAddress)
	if err != nil {
		return err
	}
	b.boardIdentity = identityBytes
	b.deviceName = stableDeviceName(b.deviceName, adapterAddress)
	trusted, bondedCount := selectBondedDevice(objects)
	b.stateMu.Lock()
	b.trustedDevice = trusted
	pairingOpen := b.pairingOpen
	pairingDeadline := b.pairingDeadline
	b.stateMu.Unlock()

	if err := b.exportObjects(); err != nil {
		return err
	}
	if err := b.configureAdapter(pairingOpen); err != nil {
		return err
	}
	if err := b.registerAgent(); err != nil {
		return err
	}
	if err := b.registerGatt(); err != nil {
		return err
	}
	if err := b.setAdvertising(true); err != nil {
		return err
	}
	// RequestDefaultAgent can make BlueZ pairable again. Reapply the service's
	// closed-by-default state after all agent and GATT registration is complete.
	if err := b.configureAdapter(pairingOpen); err != nil {
		return fmt.Errorf("reapply adapter pairing state: %w", err)
	}

	if trusted.IsValid() {
		if !variantBool(objects[trusted][blueZDeviceInterface], "Trusted") {
			if err := b.markDeviceTrusted(trusted); err != nil {
				return err
			}
		}
	}
	b.service.status.update(func(status *RuntimeStatus) {
		status.BackendAvailable = true
		status.DeviceName = b.deviceName
		status.BoardIdentity = identityText
		status.AdapterPath = string(b.adapter)
		status.AdapterAddress = adapterAddress
		status.AdapterPowered = true
		status.GattRegistered = true
		status.Advertising = true
		status.PairingOpen = pairingOpen
		status.PairingDeadline = formatDeadline(pairingDeadline)
		status.BondedDeviceCount = bondedCount
	})
	if err := b.rescanBluetoothState(); err != nil {
		b.service.status.update(func(status *RuntimeStatus) { status.LastError = err.Error() })
		if errors.Is(err, errPairingModeState) {
			return err
		}
	}
	return nil
}

func (b *blueZBackend) run(ctx context.Context) error {
	maintenance := time.NewTicker(time.Second)
	defer maintenance.Stop()
	rescanCtx, cancelRescans := context.WithCancel(ctx)
	rescanDone := make(chan struct{})
	go func() {
		defer close(rescanDone)
		b.runRescans(rescanCtx)
	}()
	defer func() {
		cancelRescans()
		<-rescanDone
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-b.conn.Context().Done():
			return errors.New("system D-Bus connection closed")
		case signal, ok := <-b.signals:
			if !ok {
				return errors.New("BlueZ signal channel closed")
			}
			b.handleSignal(signal)
		case err := <-b.fatalErrors:
			return err
		case now := <-maintenance.C:
			if err := b.expirePairingWindow(now); err != nil {
				return err
			}
			b.service.consumer.ExpireActive(now)
		}
	}
}

func (b *blueZBackend) runRescans(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-b.rescanRequests:
			if err := b.rescanBluetoothState(); err != nil {
				b.service.status.update(func(status *RuntimeStatus) { status.LastError = err.Error() })
				if errors.Is(err, errPairingModeState) {
					b.reportFatal(err)
				}
			}
		}
	}
}

func (b *blueZBackend) requestRescan() {
	select {
	case b.rescanRequests <- struct{}{}:
	default:
	}
}

func (b *blueZBackend) reportFatal(err error) {
	if err == nil {
		return
	}
	select {
	case b.fatalErrors <- err:
	default:
	}
}

func (b *blueZBackend) close() {
	if b.conn == nil {
		return
	}
	b.wakeMu.Lock()
	if b.closed {
		b.wakeMu.Unlock()
		return
	}
	b.closed = true
	b.wakeMu.Unlock()
	b.setConnectionEnabled(false)

	b.ancsMu.Lock()
	paths := b.ancs
	b.ancs = ancsPaths{}
	b.ancsMu.Unlock()

	if paths.notificationSource.IsValid() {
		_ = callWithCleanupTimeout(
			b.conn.Object(BlueZBusName, paths.notificationSource),
			blueZGattCharInterface+".StopNotify",
		).Err
	}
	if paths.dataSource.IsValid() {
		_ = callWithCleanupTimeout(
			b.conn.Object(BlueZBusName, paths.dataSource),
			blueZGattCharInterface+".StopNotify",
		).Err
	}
	b.service.consumer.ResetConnection("Bluetooth connection closed")
	if b.adapter.IsValid() {
		adapter := b.conn.Object(BlueZBusName, b.adapter)
		for _, name := range []string{"Pairable", "Discoverable"} {
			_ = callWithCleanupTimeout(adapter, dbusPropertiesInterface+".Set", blueZAdapterInterface, name, dbus.MakeVariant(false)).Err
		}
		_ = b.setAdvertising(false)
		_ = callWithCleanupTimeout(adapter, blueZGattManagerInterface+".UnregisterApplication", applicationPath).Err
	}
	_ = callWithCleanupTimeout(
		b.conn.Object(BlueZBusName, dbus.ObjectPath("/org/bluez")),
		blueZAgentManagerInterface+".UnregisterAgent",
		agentPath,
	).Err
	b.conn.RemoveSignal(b.signals)
	_ = b.conn.Close()
	b.service.status.update(func(status *RuntimeStatus) {
		status.BackendAvailable = false
		status.GattRegistered = false
		status.Advertising = false
		status.PairingOpen = false
		status.PairingDeadline = ""
		status.WakeSubscriber = false
		status.ANCSSubscribed = false
	})
}

func (b *blueZBackend) exportObjects() error {
	var err error
	_, err = b.exportGattObject(wakeServicePath, blueZGattServiceInterface, prop.Map{
		blueZGattServiceInterface: {
			"UUID":            {Value: WakeServiceUUID, Emit: prop.EmitConst},
			"Primary":         {Value: true, Emit: prop.EmitConst},
			"Characteristics": {Value: []dbus.ObjectPath{wakeCharPath}, Emit: prop.EmitConst},
		},
	}, &struct{}{})
	if err != nil {
		return fmt.Errorf("export Wake service: %w", err)
	}

	b.wakeObject = &wakeCharacteristic{backend: b}
	b.wakeProps, err = b.exportGattObject(wakeCharPath, blueZGattCharInterface, prop.Map{
		blueZGattCharInterface: {
			"UUID":    {Value: WakeCharacteristicUUID, Emit: prop.EmitConst},
			"Service": {Value: wakeServicePath, Emit: prop.EmitConst},
			// Keep the pairing probe encrypted, but use a standard notification
			// subscription. BlueZ 5.65 on the board rejects encrypt-notify CCCD
			// writes even for the bonded phone; Wake carries only a poll hint.
			"Flags":       {Value: wakeCharacteristicFlags(), Emit: prop.EmitConst},
			"Descriptors": {Value: []dbus.ObjectPath{}, Emit: prop.EmitConst},
			"Value":       {Value: []byte{}, Emit: prop.EmitTrue},
			"Notifying":   {Value: false, Emit: prop.EmitTrue},
		},
	}, b.wakeObject)
	if err != nil {
		return fmt.Errorf("export Wake characteristic: %w", err)
	}

	// Keep the legacy advertising payload within 31 bytes: flags (3), the
	// 128-bit Wake Service UUID (18), and manufacturer data containing the
	// 6-byte board identity (10). The local name is emitted in scan response.
	// Advertisement contents stay constant for the entire backend lifetime.
	// Pairing windows are enforced through Adapter1 and the pairing agent; they
	// must not remove and recreate the controller advertising set.
	b.advProps, err = prop.Export(b.conn, advertisementPath, prop.Map{
		blueZAdvertisementInterface: {
			"Type":             {Value: "peripheral", Emit: prop.EmitConst},
			"ServiceUUIDs":     {Value: advertisedServiceUUIDs(), Emit: prop.EmitConst},
			"ManufacturerData": {Value: advertisedManufacturerData(b.boardIdentity), Emit: prop.EmitConst},
			"LocalName":        {Value: b.deviceName, Emit: prop.EmitConst},
			"Discoverable":     {Value: false, Emit: prop.EmitConst},
		},
	})
	if err != nil {
		return fmt.Errorf("export BLE advertisement properties: %w", err)
	}
	advertisement := &advertisementObject{}
	if err := b.conn.Export(advertisement, advertisementPath, blueZAdvertisementInterface); err != nil {
		return fmt.Errorf("export BLE advertisement: %w", err)
	}
	agent := &pairingAgent{backend: b}
	if err := b.conn.Export(agent, agentPath, blueZAgentInterface); err != nil {
		return fmt.Errorf("export pairing agent: %w", err)
	}
	manager := &gattObjectManager{backend: b}
	if err := b.conn.Export(manager, applicationPath, dbusObjectManagerInterface); err != nil {
		return fmt.Errorf("export GATT object manager: %w", err)
	}
	if err := exportIntrospection(b.conn, applicationPath, dbusObjectManagerInterface, manager, nil); err != nil {
		return err
	}

	if err := exportIntrospection(b.conn, advertisementPath, blueZAdvertisementInterface, advertisement, b.advProps); err != nil {
		return err
	}
	return exportIntrospection(b.conn, agentPath, blueZAgentInterface, agent, nil)
}

func wakeCharacteristicFlags() []string {
	return []string{"encrypt-read", "notify"}
}

func (b *blueZBackend) exportGattObject(
	path dbus.ObjectPath,
	interfaceName string,
	properties prop.Map,
	object any,
) (*prop.Properties, error) {
	exported, err := prop.Export(b.conn, path, properties)
	if err != nil {
		return nil, err
	}
	if _, ok := object.(*struct{}); !ok {
		if aware, ok := object.(gattPropertiesAware); ok {
			aware.setGattProperties(exported)
		}
		if err := b.conn.Export(object, path, interfaceName); err != nil {
			return nil, err
		}
	}
	if err := exportIntrospection(b.conn, path, interfaceName, object, exported); err != nil {
		return nil, err
	}
	b.gattObjects = append(b.gattObjects, exportedGattObject{
		path:          path,
		interfaceName: interfaceName,
		properties:    exported,
	})
	return exported, nil
}

type gattPropertiesAware interface {
	setGattProperties(*prop.Properties)
}

func exportIntrospection(conn *dbus.Conn, path dbus.ObjectPath, interfaceName string, object any, properties *prop.Properties) error {
	iface := introspect.Interface{Name: interfaceName, Methods: introspect.Methods(object)}
	if properties != nil {
		iface.Properties = properties.Introspection(interfaceName)
	}
	node := &introspect.Node{Name: string(path), Interfaces: []introspect.Interface{iface}}
	return conn.Export(introspect.NewIntrospectable(node), path, introspect.IntrospectData.Name)
}

// signalMatchSpec describes one AddMatchSignal subscription. Kept as data so the
// scope of each subscription is assertable without a live bus.
type signalMatchSpec struct {
	name    string
	options []dbus.MatchOption
}

// signalMatchSpecs lists every BlueZ signal subscription the backend needs.
//
// The ObjectManager entry deliberately does NOT filter on a path namespace.
// BlueZ exports its ObjectManager at the root path "/" (see blueZRootPath, used
// by getManagedObjects), and InterfacesAdded/InterfacesRemoved are emitted from
// that object -- the path of the object being added travels in argument 0, not
// in the message path. A path_namespace of "/org/bluez" therefore matches
// neither, and the bus drops every one of those signals before delivery.
//
// Losing them breaks discovery of anything that appears after startup: a device
// paired later, and the ANCS GattService1/GattCharacteristic1 objects iOS only
// exposes to a bonded peer, are all announced solely via InterfacesAdded. The
// reconciler never runs while those objects exist, so notifications are never
// ingested until the service restarts and re-reads the whole tree directly.
func signalMatchSpecs() []signalMatchSpec {
	return []signalMatchSpec{
		{
			name: "properties",
			options: []dbus.MatchOption{
				dbus.WithMatchSender(BlueZBusName),
				dbus.WithMatchInterface(dbusPropertiesInterface),
				dbus.WithMatchPathNamespace(dbus.ObjectPath("/org/bluez")),
			},
		},
		{
			name: "object_manager",
			options: []dbus.MatchOption{
				dbus.WithMatchSender(BlueZBusName),
				dbus.WithMatchInterface(dbusObjectManagerInterface),
				dbus.WithMatchObjectPath(blueZRootPath),
			},
		},
		{
			name: "name_owner_changed",
			options: []dbus.MatchOption{
				dbus.WithMatchSender(dbusBusName),
				dbus.WithMatchInterface(dbusBusInterface),
				dbus.WithMatchMember("NameOwnerChanged"),
				dbus.WithMatchArg(0, BlueZBusName),
			},
		},
	}
}

func (b *blueZBackend) addSignalMatches() error {
	for _, spec := range signalMatchSpecs() {
		if err := b.conn.AddMatchSignal(spec.options...); err != nil {
			return fmt.Errorf("subscribe %s signals: %w", spec.name, err)
		}
	}
	return nil
}

func (b *blueZBackend) configureAdapter(pairingOpen bool) error {
	properties := []struct {
		name  string
		value any
	}{
		{name: "Powered", value: true},
		{name: "Alias", value: b.deviceName},
		{name: "Pairable", value: pairingOpen},
		{name: "Discoverable", value: pairingOpen},
	}
	for _, property := range properties {
		if err := callWithTimeout(
			b.conn.Object(BlueZBusName, b.adapter),
			dbusPropertiesInterface+".Set",
			blueZAdapterInterface,
			property.name,
			dbus.MakeVariant(property.value),
		).Err; err != nil {
			return fmt.Errorf("set adapter %s: %w", property.name, err)
		}
	}
	return nil
}

func (b *blueZBackend) registerAgent() error {
	manager := b.conn.Object(BlueZBusName, dbus.ObjectPath("/org/bluez"))
	if err := callWithTimeout(manager, blueZAgentManagerInterface+".RegisterAgent", agentPath, "NoInputNoOutput").Err; err != nil {
		return fmt.Errorf("register pairing agent: %w", err)
	}
	if err := callWithTimeout(manager, blueZAgentManagerInterface+".RequestDefaultAgent", agentPath).Err; err != nil {
		return fmt.Errorf("select default pairing agent: %w", err)
	}
	return nil
}

func (b *blueZBackend) registerGatt() error {
	options := map[string]dbus.Variant{}
	if err := callWithTimeout(
		b.conn.Object(BlueZBusName, b.adapter),
		blueZGattManagerInterface+".RegisterApplication",
		applicationPath,
		options,
	).Err; err != nil {
		return fmt.Errorf("register BLE GATT application: %w", err)
	}
	return nil
}

func (b *blueZBackend) setAdvertising(enabled bool) error {
	b.advertisementMu.Lock()
	defer b.advertisementMu.Unlock()
	if b.advertising == enabled {
		return nil
	}

	options := map[string]dbus.Variant{}
	manager := b.conn.Object(BlueZBusName, b.adapter)
	var err error
	if enabled {
		err = callWithTimeout(
			manager,
			blueZAdvManagerInterface+".RegisterAdvertisement",
			advertisementPath,
			options,
		).Err
		if err != nil {
			return fmt.Errorf("register BLE advertisement: %w", err)
		}
	} else {
		err = callWithTimeout(
			manager,
			blueZAdvManagerInterface+".UnregisterAdvertisement",
			advertisementPath,
		).Err
		if err != nil && !isDBusErrorNamed(err, "org.bluez.Error.DoesNotExist") {
			return fmt.Errorf("unregister BLE advertisement: %w", err)
		}
	}
	b.advertising = enabled
	b.service.status.update(func(status *RuntimeStatus) { status.Advertising = enabled })
	return nil
}

func (b *blueZBackend) getManagedObjects() (managedObjects, error) {
	objects := managedObjects{}
	if err := callWithTimeout(
		b.conn.Object(BlueZBusName, blueZRootPath),
		dbusObjectManagerInterface+".GetManagedObjects",
	).Store(&objects); err != nil {
		return nil, fmt.Errorf("read BlueZ managed objects: %w", err)
	}
	return objects, nil
}

func callWithTimeout(object dbus.BusObject, method string, args ...any) *dbus.Call {
	return callWithDeadline(object, blueZDBusCallTimeout, method, args...)
}

func callWithCleanupTimeout(object dbus.BusObject, method string, args ...any) *dbus.Call {
	return callWithDeadline(object, blueZCleanupCallTimeout, method, args...)
}

func callWithDeadline(object dbus.BusObject, timeout time.Duration, method string, args ...any) *dbus.Call {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return object.CallWithContext(ctx, method, 0, args...)
}

func isDBusErrorNamed(err error, name string) bool {
	for err != nil {
		switch typed := err.(type) {
		case dbus.Error:
			return typed.Name == name
		case *dbus.Error:
			return typed != nil && typed.Name == name
		}
		err = errors.Unwrap(err)
	}
	return false
}

func findAdapter(objects managedObjects) dbus.ObjectPath {
	preferred := dbus.ObjectPath("/org/bluez/hci0")
	if _, ok := objects[preferred][blueZAdapterInterface]; ok {
		return preferred
	}
	for path, interfaces := range objects {
		if _, ok := interfaces[blueZAdapterInterface]; ok {
			return path
		}
	}
	return ""
}

func (b *blueZBackend) handleSignal(signal *dbus.Signal) {
	if signal == nil {
		return
	}
	if signal.Name == dbusBusInterface+".NameOwnerChanged" {
		if signal.Sender != dbusBusName || len(signal.Body) < 3 || signal.Body[0] != BlueZBusName {
			return
		}
		newOwner, _ := signal.Body[2].(string)
		if newOwner != b.blueZOwner {
			b.reportFatal(errors.New("BlueZ D-Bus owner changed"))
		}
		return
	}
	if signal.Sender != BlueZBusName && signal.Sender != b.blueZOwner {
		return
	}
	switch signal.Name {
	case dbusPropertiesInterface + ".PropertiesChanged":
		b.handlePropertiesChanged(signal)
	case dbusObjectManagerInterface + ".InterfacesAdded",
		dbusObjectManagerInterface + ".InterfacesRemoved":
		b.requestRescan()
	}
}

func (b *blueZBackend) handlePropertiesChanged(signal *dbus.Signal) {
	if len(signal.Body) < 2 {
		return
	}
	interfaceName, _ := signal.Body[0].(string)
	changed, _ := signal.Body[1].(map[string]dbus.Variant)
	if interfaceName == blueZGattCharInterface {
		if value, ok := changed["Value"]; ok {
			if bytes, ok := value.Value().([]byte); ok {
				b.ancsMu.Lock()
				paths := b.ancs
				b.ancsMu.Unlock()
				switch signal.Path {
				case paths.notificationSource:
					if err := b.service.consumer.HandleNotificationSource(bytes); err != nil {
						b.service.status.update(func(status *RuntimeStatus) { status.LastError = err.Error() })
					}
				case paths.dataSource:
					if err := b.service.consumer.HandleDataSource(bytes); err != nil {
						b.service.status.update(func(status *RuntimeStatus) { status.LastError = err.Error() })
					}
				}
			}
		}
	}
	needsRescan := interfaceName == blueZDeviceInterface
	if interfaceName == blueZAdapterInterface {
		_, pairableChanged := changed["Pairable"]
		_, discoverableChanged := changed["Discoverable"]
		needsRescan = pairableChanged || discoverableChanged
	}
	if interfaceName == blueZGattCharInterface {
		_, notifyingChanged := changed["Notifying"]
		_, serviceChanged := changed["Service"]
		_, uuidChanged := changed["UUID"]
		needsRescan = notifyingChanged || serviceChanged || uuidChanged
	}
	if needsRescan {
		b.requestRescan()
	}
}

func (b *blueZBackend) rescanBluetoothState() error {
	objects, err := b.getManagedObjects()
	if err != nil {
		return err
	}
	if err := b.reconcilePairingMode(); err != nil {
		return fmt.Errorf("reconcile Bluetooth connection window: %w", err)
	}
	trusted, bondedCount, err := b.refreshTrustedDevice(objects)
	if err != nil {
		return err
	}
	var trustedProps map[string]dbus.Variant
	if trusted.IsValid() {
		trustedProps = objects[trusted][blueZDeviceInterface]
	}
	b.updateDeviceStatus(trusted, trustedProps, bondedCount)
	if !b.connectionsEnabled() {
		b.clearANCS("Bluetooth disconnected by user")
		return b.disconnectConnectedDevices(objects)
	}
	return b.rescanANCS(objects, trusted)
}

func (b *blueZBackend) rescanANCS(objects managedObjects, trustedDevice dbus.ObjectPath) error {
	if !b.connectionsEnabled() {
		b.clearANCS("Bluetooth disconnected by user")
		return nil
	}
	type candidate struct {
		paths       ancsPaths
		deviceProps map[string]dbus.Variant
	}
	var selected *candidate

	for servicePath, interfaces := range objects {
		serviceProps, ok := interfaces[blueZGattServiceInterface]
		if !ok || !strings.EqualFold(variantString(serviceProps, "UUID"), ANCSServiceUUID) {
			continue
		}
		devicePath, ok := variantObjectPath(serviceProps, "Device")
		if !ok || devicePath != trustedDevice {
			continue
		}
		deviceProps := objects[devicePath][blueZDeviceInterface]
		if !variantBool(deviceProps, "Connected") || !variantBool(deviceProps, "ServicesResolved") {
			continue
		}
		paths := ancsPaths{device: devicePath}
		for charPath, charInterfaces := range objects {
			charProps, ok := charInterfaces[blueZGattCharInterface]
			if !ok {
				continue
			}
			parentService, ok := variantObjectPath(charProps, "Service")
			if !ok || parentService != servicePath {
				continue
			}
			charUUID := variantString(charProps, "UUID")
			switch {
			case strings.EqualFold(charUUID, ANCSNotificationSourceUUID):
				paths.notificationSource = charPath
			case strings.EqualFold(charUUID, ANCSControlPointUUID):
				paths.controlPoint = charPath
			case strings.EqualFold(charUUID, ANCSDataSourceUUID):
				paths.dataSource = charPath
			}
		}
		selected = &candidate{paths: paths, deviceProps: deviceProps}
		if paths.complete() {
			break
		}
	}

	if selected == nil {
		b.clearANCS("ANCS device disconnected")
		return nil
	}
	if !selected.paths.complete() {
		b.clearANCS("ANCS characteristics are incomplete")
		return nil
	}

	b.ancsMu.Lock()
	current := b.ancs
	b.ancsMu.Unlock()
	if current == selected.paths {
		return nil
	}
	b.clearANCS("ANCS connection changed")

	if err := b.startNotify(objects, selected.paths.notificationSource); err != nil {
		return fmt.Errorf("subscribe ANCS Notification Source: %w", err)
	}
	if err := b.startNotify(objects, selected.paths.dataSource); err != nil {
		_ = callWithTimeout(
			b.conn.Object(BlueZBusName, selected.paths.notificationSource),
			blueZGattCharInterface+".StopNotify",
		).Err
		return fmt.Errorf("subscribe ANCS Data Source: %w", err)
	}

	b.ancsMu.Lock()
	b.ancs = selected.paths
	b.ancsMu.Unlock()
	b.service.consumer.SetControlPointWriter(func(command []byte) error {
		controlPoint := selected.paths.controlPoint
		command = append([]byte(nil), command...)
		object := b.conn.Object(BlueZBusName, controlPoint)
		err := callWithTimeout(
			object,
			blueZGattCharInterface+".WriteValue",
			command,
			ancsControlPointWriteOptions(),
		).Err
		if err == nil {
			return nil
		}
		writeErr := fmt.Errorf("write ANCS Control Point: %w", err)
		b.ancsMu.Lock()
		current := b.ancs.controlPoint
		b.ancsMu.Unlock()
		if current == controlPoint {
			b.service.status.update(func(status *RuntimeStatus) {
				status.LastError = writeErr.Error()
			})
		}
		return writeErr
	})
	b.service.status.update(func(status *RuntimeStatus) {
		status.ANCSSubscribed = true
		status.LastError = ""
	})
	return nil
}

func ancsControlPointWriteOptions() map[string]dbus.Variant {
	// The iOS ANCS Control Point advertises the GATT "write" flag, not
	// "write-without-response". BlueZ maps those operations to request and
	// command respectively, so the Control Point must use a write request.
	return map[string]dbus.Variant{"type": dbus.MakeVariant("request")}
}

func (b *blueZBackend) startNotify(objects managedObjects, path dbus.ObjectPath) error {
	properties := objects[path][blueZGattCharInterface]
	if variantBool(properties, "Notifying") {
		return nil
	}
	err := callWithTimeout(b.conn.Object(BlueZBusName, path), blueZGattCharInterface+".StartNotify").Err
	if isDBusErrorNamed(err, "org.bluez.Error.InProgress") {
		return nil
	}
	return err
}

func (b *blueZBackend) clearANCS(reason string) {
	b.ancsMu.Lock()
	hadConnection := b.ancs.complete()
	b.ancs = ancsPaths{}
	b.ancsMu.Unlock()
	if hadConnection {
		b.service.consumer.ResetConnection(reason)
	}
	b.service.status.update(func(status *RuntimeStatus) { status.ANCSSubscribed = false })
}

func (b *blueZBackend) NotifyWake(sequence uint64, reason string) (bool, error) {
	b.wakeMu.Lock()
	defer b.wakeMu.Unlock()
	if b.closed {
		return false, ErrBluetoothUnavailable
	}
	if b.wakeProps == nil {
		return false, ErrBluetoothUnavailable
	}
	payload := make([]byte, 12)
	payload[0] = 1
	payload[1] = wakeReasonCode(reason)
	binary.LittleEndian.PutUint64(payload[4:], sequence)
	notifying, _ := b.wakeProps.GetMust(blueZGattCharInterface, "Notifying").(bool)
	b.wakeProps.SetMust(blueZGattCharInterface, "Value", payload)
	return notifying, nil
}

func (b *blueZBackend) wakeSubscribed() bool {
	b.wakeMu.Lock()
	defer b.wakeMu.Unlock()
	if b.wakeProps == nil {
		return false
	}
	notifying, _ := b.wakeProps.GetMust(blueZGattCharInterface, "Notifying").(bool)
	return notifying
}

func wakeReasonCode(reason string) byte {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "queue", "phone_bridge", "command":
		return 1
	case "manual", "user":
		return 2
	case "system", "notification":
		return 3
	case "live_activity":
		return 4
	case "usb_reenumeration":
		return 5
	default:
		return 0
	}
}

type gattObjectManager struct {
	backend *blueZBackend
}

func (m *gattObjectManager) GetManagedObjects() (managedObjects, *dbus.Error) {
	objects := make(managedObjects, len(m.backend.gattObjects))
	for _, object := range m.backend.gattObjects {
		properties, err := object.properties.GetAll(object.interfaceName)
		if err != nil {
			return nil, err
		}
		objects[object.path] = map[string]map[string]dbus.Variant{
			object.interfaceName: properties,
		}
	}
	return objects, nil
}

type wakeCharacteristic struct {
	backend    *blueZBackend
	properties *prop.Properties
}

func readValueAtOffset(value []byte, options map[string]dbus.Variant) ([]byte, *dbus.Error) {
	offset := uint16(0)
	if variant, ok := options["offset"]; ok {
		parsed, ok := variant.Value().(uint16)
		if !ok {
			return nil, dbus.NewError("org.bluez.Error.InvalidArguments", []any{"offset must be uint16"})
		}
		offset = parsed
	}
	if int(offset) > len(value) {
		return nil, dbus.NewError(
			"org.bluez.Error.InvalidOffset",
			[]any{fmt.Sprintf("offset %d exceeds value length %d", offset, len(value))},
		)
	}
	return append([]byte(nil), value[offset:]...), nil
}

func (c *wakeCharacteristic) setGattProperties(properties *prop.Properties) {
	c.properties = properties
	c.backend.wakeProps = properties
}

func (c *wakeCharacteristic) ReadValue(options map[string]dbus.Variant) ([]byte, *dbus.Error) {
	// The characteristic is encrypt-read, so BlueZ forwards a read only after
	// the link is encrypted with a valid bond. This is the authentication
	// signal; a plain notify subscription is not.
	c.backend.wakeMu.Lock()
	value, _ := c.properties.GetMust(blueZGattCharInterface, "Value").([]byte)
	c.backend.wakeMu.Unlock()
	result, err := readValueAtOffset(value, options)
	if err != nil {
		return nil, err
	}
	logging.Infof("ble_service", "bluez", "BLE Wake authenticated read device=%s", variantDevicePath(options))
	c.backend.recordWakeAuthenticatedRead(time.Now())
	return result, nil
}

func (c *wakeCharacteristic) StartNotify() *dbus.Error {
	now := time.Now()
	c.backend.wakeMu.Lock()
	if notifying, _ := c.properties.GetMust(blueZGattCharInterface, "Notifying").(bool); notifying {
		c.backend.lastWakeNotifyStart = now
	} else {
		c.properties.SetMust(blueZGattCharInterface, "Notifying", true)
		c.backend.lastWakeNotifyStart = now
	}
	c.backend.wakeMu.Unlock()
	logging.Infof("ble_service", "bluez", "BLE Wake StartNotify")
	c.backend.service.status.update(func(status *RuntimeStatus) { status.WakeSubscriber = true })
	c.backend.recordWakeSubscribed(now)
	c.backend.requestRescan()
	return nil
}

// recordWakeSubscribed and recordWakeAuthenticatedRead are the window-relevant
// parts of StartNotify and a successful ReadValue. They are kept free of D-Bus
// property access so the window rule can be exercised without a bus.
func (b *blueZBackend) recordWakeSubscribed(now time.Time) {
	b.wakeMu.Lock()
	b.wakeSubscribedAt = now
	b.wakeMu.Unlock()
	b.maybeFinishConnectionWindow("start_notify")
}

func (b *blueZBackend) recordWakeAuthenticatedRead(now time.Time) {
	b.wakeMu.Lock()
	b.wakeAuthenticatedAt = now
	b.wakeMu.Unlock()
	b.maybeFinishConnectionWindow("authenticated_read")
}

// connectionWindowComplete reports whether the phone finished the App
// connection inside the window opened at windowOpenedAt. The Wake CCCD has no
// encryption requirement, so iOS can subscribe (or restore a cached CCCD)
// before it can decrypt the link, for example when it has forgotten the board
// but BlueZ still holds the old key. Closing the window then makes the kernel
// refuse the phone's following SMP Pairing Request with "Pairing not
// supported". Require both the subscription and an encrypted Wake read inside
// the same window, in either order; a subscription left over from an earlier
// window does not count.
func connectionWindowComplete(windowOpenedAt, authenticatedAt, subscribedAt time.Time) bool {
	return happenedInWindow(windowOpenedAt, authenticatedAt) &&
		happenedInWindow(windowOpenedAt, subscribedAt)
}

func happenedInWindow(windowOpenedAt, at time.Time) bool {
	return !at.IsZero() && !at.Before(windowOpenedAt)
}

func (b *blueZBackend) maybeFinishConnectionWindow(trigger string) {
	b.stateMu.Lock()
	open := b.pairingOpen
	openedAt := b.pairingOpenedAt
	b.stateMu.Unlock()
	if !open {
		return
	}
	b.wakeMu.Lock()
	authenticatedAt := b.wakeAuthenticatedAt
	subscribedAt := b.wakeSubscribedAt
	b.wakeMu.Unlock()
	if !connectionWindowComplete(openedAt, authenticatedAt, subscribedAt) {
		logging.Infof(
			"ble_service", "pairing",
			"BLE pairing window kept open trigger=%s subscribed=%t authenticated=%t",
			trigger, happenedInWindow(openedAt, subscribedAt), happenedInWindow(openedAt, authenticatedAt),
		)
		return
	}
	go func() {
		closed, err := b.closePairingWindowOpenedAt(openedAt)
		if closed {
			logging.Infof("ble_service", "pairing", "BLE pairing window closed reason=app_connected trigger=%s", trigger)
		}
		if err != nil {
			b.service.status.update(func(status *RuntimeStatus) { status.LastError = err.Error() })
		}
	}()
}

func (c *wakeCharacteristic) StopNotify() *dbus.Error {
	now := time.Now()
	connected := c.backend.service.Status().Connected
	c.backend.wakeMu.Lock()
	if shouldIgnoreWakeStop(connected, c.backend.lastWakeNotifyStart, now) {
		age := now.Sub(c.backend.lastWakeNotifyStart)
		c.backend.wakeMu.Unlock()
		logging.Debugf("ble_service", "bluez", "BLE Wake ignored stale StopNotify age=%s", age.Round(time.Millisecond))
		return nil
	}
	c.properties.SetMust(blueZGattCharInterface, "Notifying", false)
	c.backend.lastWakeNotifyStart = time.Time{}
	c.backend.wakeSubscribedAt = time.Time{}
	c.backend.wakeMu.Unlock()
	logging.Infof("ble_service", "bluez", "BLE Wake StopNotify")
	c.backend.service.status.update(func(status *RuntimeStatus) { status.WakeSubscriber = false })
	c.backend.requestRescan()
	return nil
}

func shouldIgnoreWakeStop(connected bool, lastStart, now time.Time) bool {
	return connected && !lastStart.IsZero() && now.Sub(lastStart) >= 0 &&
		now.Sub(lastStart) < wakeStopGracePeriod
}

type advertisementObject struct{}

func (a *advertisementObject) Release() *dbus.Error { return nil }

type pairingAgent struct {
	backend *blueZBackend
}

func (a *pairingAgent) Release() *dbus.Error { return nil }
func (a *pairingAgent) RequestPinCode(dbus.ObjectPath) (string, *dbus.Error) {
	return "", dbus.NewError("org.bluez.Error.Rejected", []any{"legacy PIN pairing is unsupported"})
}
func (a *pairingAgent) DisplayPinCode(dbus.ObjectPath, string) *dbus.Error { return nil }
func (a *pairingAgent) RequestPasskey(dbus.ObjectPath) (uint32, *dbus.Error) {
	return 0, dbus.NewError("org.bluez.Error.Rejected", []any{"legacy passkey pairing is unsupported"})
}
func (a *pairingAgent) DisplayPasskey(dbus.ObjectPath, uint32, uint16) *dbus.Error {
	return nil
}
func (a *pairingAgent) RequestConfirmation(device dbus.ObjectPath, _ uint32) *dbus.Error {
	return a.authorize("RequestConfirmation", device)
}
func (a *pairingAgent) RequestAuthorization(device dbus.ObjectPath) *dbus.Error {
	return a.authorize("RequestAuthorization", device)
}
func (a *pairingAgent) AuthorizeService(device dbus.ObjectPath, _ string) *dbus.Error {
	return a.authorize("AuthorizeService", device)
}
func (a *pairingAgent) Cancel() *dbus.Error {
	logging.Infof("ble_service", "agent", "BLE pairing agent request cancelled")
	return nil
}

func (a *pairingAgent) authorize(method string, device dbus.ObjectPath) *dbus.Error {
	if a.backend != nil && a.backend.deviceAllowed(device) {
		logging.Infof("ble_service", "agent", "BLE pairing agent allowed method=%s device=%s", method, device)
		return nil
	}
	logging.Warnf("ble_service", "agent", "BLE pairing agent rejected method=%s device=%s", method, device)
	return dbus.NewError("org.bluez.Error.Rejected", []any{"only the trusted iPhone may use Aiden BLE"})
}

func variantDevicePath(options map[string]dbus.Variant) dbus.ObjectPath {
	if path, ok := variantObjectPath(options, "device"); ok {
		return path
	}
	return ""
}

func variantString(properties map[string]dbus.Variant, name string) string {
	if variant, ok := properties[name]; ok {
		if value, ok := variant.Value().(string); ok {
			return value
		}
	}
	return ""
}

func variantBool(properties map[string]dbus.Variant, name string) bool {
	if variant, ok := properties[name]; ok {
		if value, ok := variant.Value().(bool); ok {
			return value
		}
	}
	return false
}

func variantObjectPath(properties map[string]dbus.Variant, name string) (dbus.ObjectPath, bool) {
	if variant, ok := properties[name]; ok {
		value, ok := variant.Value().(dbus.ObjectPath)
		return value, ok && value.IsValid()
	}
	return "", false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
