package backup

import (
	"fmt"
	"path/filepath"
	"sort"
)

type Roots struct {
	Userdata string
	SD       string
}

func DefaultRoots() Roots {
	return Roots{Userdata: "/userdata", SD: "/mnt/sdcard"}
}

func (r Roots) Validate() error {
	for name, value := range map[string]string{"userdata": r.Userdata, "sd": r.SD} {
		if !filepath.IsAbs(value) || filepath.Clean(value) != value {
			return fmt.Errorf("%s root must be an absolute clean path", name)
		}
	}
	return nil
}

type SourceKind int

const (
	SourceFile SourceKind = iota
	SourceDirectory
	SourceOTASettings
)

type SourceSpec struct {
	Path        string
	ArchivePath string
	Kind        SourceKind
	Optional    bool
	Exclude     []string
	SD          bool
}

type ComponentDefinition struct {
	ID              ComponentID
	SchemaVersion   int
	DefaultSelected bool
	Sensitive       bool
	Advanced        bool
	SameDeviceOnly  bool
	RequiresSD      bool
	Sources         func(Roots) []SourceSpec
}

func ComponentDefinitions() []ComponentDefinition {
	return []ComponentDefinition{
		{ID: ComponentAgentConfig, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "agent/agent.toml"), ArchivePath: "agent.toml", Kind: SourceFile}}
		}},
		{ID: ComponentSystemEnvironment, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "system/env"), ArchivePath: "env", Kind: SourceFile, Optional: true}}
		}},
		{ID: ComponentNetwork, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{
				{Path: filepath.Join(r.Userdata, "debian/wifi/wpa_supplicant-wlan0.conf"), ArchivePath: "wpa_supplicant-wlan0.conf", Kind: SourceFile, Optional: true},
				{Path: filepath.Join(r.Userdata, "system/wifi-proxies.json"), ArchivePath: "wifi-proxies.json", Kind: SourceFile, Optional: true},
				{Path: filepath.Join(r.Userdata, "system/wifi-region.json"), ArchivePath: "wifi-region.json", Kind: SourceFile, Optional: true},
			}
		}},
		{ID: ComponentOTASettings, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "debian/ota/config.json"), ArchivePath: "settings.json", Kind: SourceOTASettings, Optional: true}}
		}},
		{ID: ComponentAgentSkills, SchemaVersion: 1, DefaultSelected: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{
				{Path: filepath.Join(r.Userdata, "agent/skills"), ArchivePath: "skills", Kind: SourceDirectory, Optional: true},
				{Path: filepath.Join(r.Userdata, "agent/skill-state"), ArchivePath: "skill-state", Kind: SourceDirectory, Optional: true},
			}
		}},
		{ID: ComponentAgentMemory, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "agent/memory"), Kind: SourceDirectory, Optional: true}}
		}},
		{ID: ComponentAgentSessions, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "agent/sessions"), Kind: SourceDirectory, Optional: true}}
		}},
		{ID: ComponentUserHome, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "userhome"), Kind: SourceDirectory, Optional: true}}
		}},
		{ID: ComponentPreferences, SchemaVersion: 1, DefaultSelected: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "audio_service/playback_volume"), ArchivePath: "audio_service/playback_volume", Kind: SourceFile, Optional: true}}
		}},
		{ID: ComponentDeviceIdentity, SchemaVersion: 1, DefaultSelected: true, Sensitive: true, SameDeviceOnly: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{
				{Path: filepath.Join(r.Userdata, "system/machine-id"), ArchivePath: "machine-id", Kind: SourceFile, Optional: true},
				{Path: filepath.Join(r.Userdata, "system/ssh"), ArchivePath: "ssh", Kind: SourceDirectory, Optional: true},
				{Path: filepath.Join(r.Userdata, "ble_service/bluetooth"), ArchivePath: "bluetooth", Kind: SourceDirectory, Optional: true},
			}
		}},
		{ID: ComponentAudioArchive, SchemaVersion: 1, DefaultSelected: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "audio"), Kind: SourceDirectory, Optional: true}}
		}},
		{ID: ComponentSDManagedAudio, SchemaVersion: 1, DefaultSelected: true, RequiresSD: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.SD, "aico/audio"), Kind: SourceDirectory, Optional: true, SD: true, Exclude: []string{"**/*.aico-partial"}}}
		}},
		{ID: ComponentSDUserFiles, SchemaVersion: 1, Advanced: true, RequiresSD: true, Sources: func(r Roots) []SourceSpec {
			// The planner excludes managed roots through IsReservedSDUserPath,
			// matching restore's FAT-aware boundary instead of glob spelling.
			return []SourceSpec{{Path: r.SD, Kind: SourceDirectory, Optional: true, SD: true, Exclude: []string{"lost+found", "lost+found/**", "**/*.aico-partial"}}}
		}},
		{ID: ComponentPythonEnvironment, SchemaVersion: 1, Advanced: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{{Path: filepath.Join(r.Userdata, "agent/python"), Kind: SourceDirectory, Optional: true}}
		}},
		{ID: ComponentDiagnostics, SchemaVersion: 1, Advanced: true, Sensitive: true, Sources: func(r Roots) []SourceSpec {
			return []SourceSpec{
				{Path: filepath.Join(r.Userdata, "agent/log"), ArchivePath: "agent", Kind: SourceDirectory, Optional: true},
				{Path: filepath.Join(r.Userdata, "log"), ArchivePath: "system", Kind: SourceDirectory, Optional: true},
			}
		}},
	}
}

func DefinitionsByID() map[ComponentID]ComponentDefinition {
	definitions := ComponentDefinitions()
	result := make(map[ComponentID]ComponentDefinition, len(definitions))
	for _, definition := range definitions {
		result[definition.ID] = definition
	}
	return result
}

func DefaultComponents(mode Mode, sdAvailable bool) []ComponentID {
	var result []ComponentID
	for _, definition := range ComponentDefinitions() {
		if definition.SameDeviceOnly && mode != ModeSameDevice {
			continue
		}
		if definition.RequiresSD && !sdAvailable {
			continue
		}
		result = append(result, definition.ID)
	}
	return result
}

func ValidateSelection(mode Mode, selected []ComponentID, sdAvailable bool) ([]ComponentDefinition, error) {
	if err := mode.Validate(); err != nil {
		return nil, err
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("at least one backup component is required")
	}
	known := DefinitionsByID()
	seen := make(map[ComponentID]bool, len(selected))
	result := make([]ComponentDefinition, 0, len(selected))
	for _, id := range selected {
		definition, ok := known[id]
		if !ok {
			return nil, fmt.Errorf("unknown backup component %q", id)
		}
		if seen[id] {
			return nil, fmt.Errorf("duplicate backup component %q", id)
		}
		if definition.SameDeviceOnly && mode != ModeSameDevice {
			return nil, fmt.Errorf("component %s is not allowed in portable mode", id)
		}
		if definition.RequiresSD && !sdAvailable {
			return nil, fmt.Errorf("component %s requires a mounted SD card", id)
		}
		seen[id] = true
		result = append(result, definition)
	}
	order := make(map[ComponentID]int, len(CommitOrder))
	for index, id := range CommitOrder {
		order[id] = index
	}
	sort.Slice(result, func(i, j int) bool {
		left, leftOK := order[result[i].ID]
		right, rightOK := order[result[j].ID]
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && left != right {
			return left < right
		}
		return result[i].ID < result[j].ID
	})
	return result, nil
}
