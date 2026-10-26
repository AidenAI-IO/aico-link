package ble

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestAdvertisedServiceUUIDs(t *testing.T) {
	want := []string{WakeServiceUUID}
	if got := advertisedServiceUUIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("advertisedServiceUUIDs() = %#v, want %#v", got, want)
	}
}

func TestApplyAdapterPairingModeOnlyChangesAdapterProperties(t *testing.T) {
	type propertyUpdate struct {
		name  string
		value bool
	}
	updates := make([]propertyUpdate, 0, 2)
	if err := applyAdapterPairingMode(true, func(name string, value bool) error {
		updates = append(updates, propertyUpdate{name: name, value: value})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []propertyUpdate{
		{name: "Pairable", value: true},
		{name: "Discoverable", value: true},
	}
	if !reflect.DeepEqual(updates, want) {
		t.Fatalf("pairing mode updates = %#v, want %#v", updates, want)
	}
}

func TestApplyAdapterPairingModeStopsAfterPropertyFailure(t *testing.T) {
	wantErr := errors.New("set failed")
	updates := make([]string, 0, 2)
	err := applyAdapterPairingMode(false, func(name string, _ bool) error {
		updates = append(updates, name)
		if name == "Pairable" {
			return wantErr
		}
		return nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("applyAdapterPairingMode() error = %v, want %v", err, wantErr)
	}
	if want := []string{"Pairable"}; !reflect.DeepEqual(updates, want) {
		t.Fatalf("property updates after failure = %#v, want %#v", updates, want)
	}
}

func TestBoardIdentityUsesFullAdapterAddress(t *testing.T) {
	text, identity, err := boardIdentityFromAdapterAddress("AA:BB:CC:DD:12:34")
	if err != nil {
		t.Fatal(err)
	}
	if text != "aabbccdd1234" || !bytes.Equal(identity, []byte{0xaa, 0xbb, 0xcc, 0xdd, 0x12, 0x34}) {
		t.Fatalf("unexpected board identity text=%q bytes=%x", text, identity)
	}
	manufacturerData := advertisedManufacturerData(identity)
	variant, ok := manufacturerData[AicoLinkManufacturerID]
	if !ok || !bytes.Equal(variant.Value().([]byte), identity) {
		t.Fatalf("advertisement does not carry board identity: %#v", manufacturerData)
	}
}

func TestBoardIdentityRejectsInvalidAdapterAddress(t *testing.T) {
	if _, _, err := boardIdentityFromAdapterAddress("12:34"); err == nil {
		t.Fatal("short adapter address was accepted")
	}
}

func TestDisconnectedDeviceClearsSubscriberStatus(t *testing.T) {
	service := NewService(1)
	service.status.update(func(status *RuntimeStatus) {
		status.WakeSubscriber = true
		status.ANCSSubscribed = true
	})
	backend := newBlueZBackend(service, "Aico Link", 0)

	backend.updateDeviceStatus(
		dbus.ObjectPath("/org/bluez/hci0/dev_00_11_22_33_44_55"),
		map[string]dbus.Variant{
			"Name":      dbus.MakeVariant("iPhone"),
			"Address":   dbus.MakeVariant("00:11:22:33:44:55"),
			"Connected": dbus.MakeVariant(false),
		},
		1,
	)

	status := service.Status()
	if !status.Paired || status.Connected || status.WakeSubscriber || status.ANCSSubscribed {
		t.Fatalf("unexpected disconnected status: %#v", status)
	}
}

func TestUserDisabledConnectionStaysDisconnectedWhenLinkReturns(t *testing.T) {
	service := NewService(1)
	service.status.update(func(status *RuntimeStatus) {
		status.WakeSubscriber = true
		status.ANCSSubscribed = true
	})
	backend := newBlueZBackend(service, "Aico Link", 0)
	backend.setConnectionEnabled(false)

	backend.updateDeviceStatus(
		dbus.ObjectPath("/org/bluez/hci0/dev_00_11_22_33_44_55"),
		map[string]dbus.Variant{
			"Name":             dbus.MakeVariant("iPhone"),
			"Address":          dbus.MakeVariant("00:11:22:33:44:55"),
			"Connected":        dbus.MakeVariant(true),
			"ServicesResolved": dbus.MakeVariant(true),
		},
		1,
	)

	status := service.Status()
	if !status.Paired || status.Connected || status.ServicesResolved ||
		status.WakeSubscriber || status.ANCSSubscribed {
		t.Fatalf("user-disabled connection became active again: %#v", status)
	}
}

func TestConnectedDevicePathsIncludesPairedAndUnpairedLinks(t *testing.T) {
	objects := managedObjects{
		dbus.ObjectPath("/org/bluez/hci0/dev_02"): {
			blueZDeviceInterface: {
				"Connected": dbus.MakeVariant(true),
				"Paired":    dbus.MakeVariant(false),
			},
		},
		dbus.ObjectPath("/org/bluez/hci0/dev_01"): {
			blueZDeviceInterface: {
				"Connected": dbus.MakeVariant(true),
				"Paired":    dbus.MakeVariant(true),
			},
		},
		dbus.ObjectPath("/org/bluez/hci0/dev_03"): {
			blueZDeviceInterface: {
				"Connected": dbus.MakeVariant(false),
			},
		},
	}
	want := []dbus.ObjectPath{
		dbus.ObjectPath("/org/bluez/hci0/dev_01"),
		dbus.ObjectPath("/org/bluez/hci0/dev_02"),
	}
	if got := connectedDevicePaths(objects); !reflect.DeepEqual(got, want) {
		t.Fatalf("connectedDevicePaths() = %v, want %v", got, want)
	}
}
