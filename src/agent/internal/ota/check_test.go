package ota

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAvailabilityChecksMetadataWithoutMutatingDevice(t *testing.T) {
	for _, mode := range []string{"available", "current", "bad-signature", "pending", "factory"} {
		t.Run(mode, func(t *testing.T) {
			env := newUpdaterTestEnv(t)
			if mode == "current" {
				env.state.LastCommittedVersion, env.state.LastCommittedBuildTime = env.version, env.buildTime
				env.saveState(t)
			}
			manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("new-a"), "boot_b.img": []byte("new-b"), "rootfs.img": []byte("new-root")}, nil)
			if mode == "bad-signature" {
				manifest = []byte(`{"signature":"invalid"}`)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/manifest.json" {
					t.Errorf("check downloaded an image: %s", r.URL.Path)
				}
				_, _ = w.Write(manifest)
			}))
			defer server.Close()
			env.config.ManifestURL = server.URL + "/manifest.json"
			if mode == "pending" {
				if err := os.WriteFile(filepath.Join(env.stateDir, "pending_boot.json"), []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "factory" {
				env.config.FactoryVersion, env.config.FactoryBuildTime = env.state.LastCommittedVersion, env.state.LastCommittedBuildTime
				if err := os.Remove(filepath.Join(env.stateDir, "state.json")); err != nil {
					t.Fatal(err)
				}
			}
			paths := []string{env.miscPath, filepath.Join(env.stateDir, "state.json"), filepath.Join(env.blockDir, "boot_b"), filepath.Join(env.blockDir, "rootfs_b")}
			before := map[string]string{}
			for _, path := range paths {
				data, _ := os.ReadFile(path)
				before[path] = string(data)
			}
			result, err := env.updater().CheckAvailable(context.Background())
			if mode == "pending" || mode == "bad-signature" {
				if err == nil {
					t.Fatal("invalid or pending update accepted")
				}
			} else if err != nil || result.Available != (mode != "current") || result.Version != env.version {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			for _, path := range paths {
				data, _ := os.ReadFile(path)
				if string(data) != before[path] {
					t.Errorf("check changed %s", path)
				}
			}
			if env.reboots != 0 {
				t.Fatal("check rebooted device")
			}
			if mode == "factory" {
				if _, err := os.Stat(filepath.Join(env.stateDir, "state.json")); !os.IsNotExist(err) {
					t.Fatal("check initialized transaction state")
				}
			}
		})
	}
}

func TestUpdateRejectsReleaseChangedSinceCheck(t *testing.T) {
	env := newUpdaterTestEnv(t)
	manifest := env.signedManifest(map[string][]byte{"boot_a.img": []byte("new-a"), "boot_b.img": []byte("new-b"), "rootfs.img": []byte("new-root")}, nil)
	server := env.releaseServer(t, manifest, nil)
	env.config.ReleaseURL = server.URL + "/repos/AidenAI-IO/aico-link/releases/latest"
	env.config.ExpectedVersion = "different-version"
	_, err := env.updater().CheckOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "changed since availability check") {
		t.Fatalf("err=%v", err)
	}
	state, loadErr := LoadState(filepath.Join(env.stateDir, "state.json"))
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state.Phase != "manifest" || state.LastError != err.Error() {
		t.Fatalf("mismatch not recorded in status: %+v", state)
	}
	assertFileContent(t, filepath.Join(env.blockDir, "boot_a"), "old-boot-a")
	if env.reboots != 0 {
		t.Fatal("mismatched release rebooted")
	}
}
