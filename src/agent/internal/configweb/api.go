package configweb

import (
	"net/http"
	"strings"
)

// Config Web deliberately uses the same /api namespace as the Agent runtime.
// Resource nouns provide the boundary; the API does not add a version prefix.
const apiPrefix = "/api"

type apiEndpoint int

const (
	apiUnknown apiEndpoint = iota
	apiDeviceSnapshot
	apiConfigGet
	apiConfigSchema
	apiConfigUpdate
	apiConfigPlan
	apiConfigLocale
	apiConfigApplication
	apiConfigTest
	apiMemoryReset
	apiConversationReset
	apiModels
	apiSTTTestStart
	apiSTTTestStop
	apiStorageStatus
	apiStorageFormat
	apiStorageEject
	apiConfigBackupExport
	apiConfigBackupImport
	apiWiFiScan
	apiWiFiConnect
	apiWiFiConnectStatus
	apiWiFiForget
	apiWiFiRegionGet
	apiWiFiRegionResolve
	apiWiFiRegionUpdate
	apiSystemEnvironmentGet
	apiSystemEnvironmentPut
	apiSystemEnvironmentApply
	apiDeviceStatus
	apiAgentLogs
	apiOTAStatus
	apiOTAUpdate
	apiDeviceReboot
	apiAgentRestart
	apiUSBReenumerate
	apiSupportArchive
	apiLLMLogs
	apiLLMLogExport
	apiLLMLogImport
	apiMaintenanceSession
	apiMaintenanceCurrent
	apiBackupCapabilities
	apiBackupJobs
	apiBackupJob
	apiBackupArchive
	apiRestoreJobs
	apiRestoreJob
	apiRestoreChunk
	apiRestorePlan
	apiRestoreValidate
	apiRestoreApply
)

type routeVariant struct{ method, path string }

type apiRoute struct {
	endpoint  apiEndpoint
	canonical routeVariant
}

var apiRoutes = []apiRoute{
	{apiDeviceSnapshot, routeVariant{http.MethodGet, apiPrefix + "/device/snapshot"}},
	{apiConfigApplication, routeVariant{http.MethodGet, apiPrefix + "/config/application"}},
	{apiConfigGet, routeVariant{http.MethodGet, apiPrefix + "/config"}},
	{apiConfigSchema, routeVariant{http.MethodGet, apiPrefix + "/config/schema"}},
	{apiConfigUpdate, routeVariant{http.MethodPatch, apiPrefix + "/config"}},
	{apiConfigPlan, routeVariant{http.MethodPost, apiPrefix + "/config/plan"}},
	{apiConfigLocale, routeVariant{http.MethodPut, apiPrefix + "/config/locale"}},
	{apiConfigTest, routeVariant{http.MethodPost, apiPrefix + "/config/test"}},
	{apiMemoryReset, routeVariant{http.MethodPost, apiPrefix + "/memory/reset"}},
	{apiConversationReset, routeVariant{http.MethodPost, apiPrefix + "/conversation/reset"}},
	{apiModels, routeVariant{http.MethodGet, apiPrefix + "/models"}},
	{apiSTTTestStart, routeVariant{http.MethodPost, apiPrefix + "/config-test/stt/start"}},
	{apiSTTTestStop, routeVariant{http.MethodPost, apiPrefix + "/config-test/stt/stop"}},
	{apiStorageStatus, routeVariant{http.MethodGet, apiPrefix + "/storage/status"}},
	{apiStorageFormat, routeVariant{http.MethodPost, apiPrefix + "/storage/format"}},
	{apiStorageEject, routeVariant{http.MethodPost, apiPrefix + "/storage/eject"}},
	{apiConfigBackupExport, routeVariant{http.MethodGet, apiPrefix + "/config/backup"}},
	{apiConfigBackupImport, routeVariant{http.MethodPut, apiPrefix + "/config/backup"}},
	{apiWiFiScan, routeVariant{http.MethodPost, apiPrefix + "/network/wifi/scan"}},
	{apiWiFiConnect, routeVariant{http.MethodPut, apiPrefix + "/network/wifi/connection"}},
	{apiWiFiConnectStatus, routeVariant{http.MethodGet, apiPrefix + "/network/wifi/connection"}},
	{apiWiFiForget, routeVariant{http.MethodDelete, apiPrefix + "/network/wifi/connection"}},
	{apiWiFiRegionGet, routeVariant{http.MethodGet, apiPrefix + "/network/wifi/region"}},
	{apiWiFiRegionResolve, routeVariant{http.MethodPost, apiPrefix + "/network/wifi/region/resolve"}},
	{apiWiFiRegionUpdate, routeVariant{http.MethodPut, apiPrefix + "/network/wifi/region"}},
	{apiSystemEnvironmentGet, routeVariant{http.MethodGet, apiPrefix + "/system/environment"}},
	{apiSystemEnvironmentPut, routeVariant{http.MethodPut, apiPrefix + "/system/environment"}},
	{apiSystemEnvironmentApply, routeVariant{http.MethodPost, apiPrefix + "/system/environment/apply"}},
	{apiDeviceStatus, routeVariant{http.MethodGet, apiPrefix + "/device/status"}},
	{apiAgentLogs, routeVariant{http.MethodGet, apiPrefix + "/logs/agent"}},
	{apiOTAStatus, routeVariant{http.MethodGet, apiPrefix + "/ota/status"}},
	{apiOTAUpdate, routeVariant{http.MethodPost, apiPrefix + "/ota/updates"}},
	{apiDeviceReboot, routeVariant{http.MethodPost, apiPrefix + "/device/reboot"}},
	{apiAgentRestart, routeVariant{http.MethodPost, apiPrefix + "/agent/restart"}},
	{apiUSBReenumerate, routeVariant{http.MethodPost, apiPrefix + "/device/usb/reenumerate"}},
	{apiSupportArchive, routeVariant{http.MethodGet, apiPrefix + "/logs/support"}},
	{apiLLMLogs, routeVariant{http.MethodGet, apiPrefix + "/logs/llm"}},
	{apiMaintenanceSession, routeVariant{http.MethodPost, apiPrefix + "/maintenance/sessions"}},
	{apiMaintenanceCurrent, routeVariant{http.MethodGet, apiPrefix + "/maintenance/current"}},
	{apiBackupCapabilities, routeVariant{http.MethodGet, apiPrefix + "/backup/capabilities"}},
	{apiBackupJobs, routeVariant{http.MethodPost, apiPrefix + "/backup/jobs"}},
	{apiRestoreJobs, routeVariant{http.MethodPost, apiPrefix + "/restore/jobs"}},
}

type apiMatch struct {
	endpoint apiEndpoint
	suffix   string
}

func (s *Server) APIHandler() http.Handler { return http.HandlerFunc(s.serveAPI) }

func matchAPIRequest(r *http.Request) apiMatch {
	for _, route := range apiRoutes {
		if r.Method == route.canonical.method && r.URL.Path == route.canonical.path {
			return apiMatch{endpoint: route.endpoint}
		}
	}
	const backupJobPrefix = apiPrefix + "/backup/jobs/"
	if strings.HasPrefix(r.URL.Path, backupJobPrefix) {
		suffix := strings.TrimPrefix(r.URL.Path, backupJobPrefix)
		if suffix != "" && !strings.Contains(suffix, "/") {
			switch r.Method {
			case http.MethodGet, http.MethodDelete:
				return apiMatch{endpoint: apiBackupJob, suffix: suffix}
			}
		}
		if jobID, ok := strings.CutSuffix(suffix, "/archive"); ok && jobID != "" && !strings.Contains(jobID, "/") && r.Method == http.MethodGet {
			return apiMatch{endpoint: apiBackupArchive, suffix: jobID}
		}
	}
	const restoreJobPrefix = apiPrefix + "/restore/jobs/"
	if strings.HasPrefix(r.URL.Path, restoreJobPrefix) {
		suffix := strings.TrimPrefix(r.URL.Path, restoreJobPrefix)
		parts := strings.Split(suffix, "/")
		if len(parts) == 1 && parts[0] != "" {
			switch r.Method {
			case http.MethodGet, http.MethodDelete:
				return apiMatch{endpoint: apiRestoreJob, suffix: parts[0]}
			}
		}
		if len(parts) == 3 && parts[0] != "" {
			switch parts[1] {
			case "chunks":
				if r.Method == http.MethodPut && parts[2] != "" {
					return apiMatch{endpoint: apiRestoreChunk, suffix: parts[0] + "/" + parts[2]}
				}
			}
		}
		if len(parts) == 2 && parts[0] != "" && parts[1] != "" && r.Method == http.MethodPost {
			switch parts[1] {
			case "plan":
				return apiMatch{endpoint: apiRestorePlan, suffix: parts[0]}
			case "validate":
				return apiMatch{endpoint: apiRestoreValidate, suffix: parts[0]}
			case "apply":
				return apiMatch{endpoint: apiRestoreApply, suffix: parts[0]}
			}
		}
	}
	escapedPath := r.URL.EscapedPath()
	const canonicalLogPrefix = apiPrefix + "/logs/llm/"
	if strings.HasPrefix(escapedPath, canonicalLogPrefix) {
		suffix := strings.TrimPrefix(escapedPath, canonicalLogPrefix)
		switch r.Method {
		case http.MethodGet:
			return apiMatch{endpoint: apiLLMLogExport, suffix: suffix}
		case http.MethodPut:
			return apiMatch{endpoint: apiLLMLogImport, suffix: suffix}
		}
	}
	return apiMatch{}
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	match := matchAPIRequest(r)
	if match.endpoint == apiUnknown {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	if s.maintenance != nil && s.maintenance.active() && endpointConflictsWithMaintenance(match.endpoint) {
		s.writeMaintenanceLocked(w)
		return
	}
	s.startDeferredRestartIfIdle()
	w.Header().Set("X-Aico-API-Version", "1")
	switch match.endpoint {
	case apiDeviceSnapshot:
		s.handleGetDeviceSnapshot(w, r)
	case apiConfigApplication:
		s.handleConfigApplication(w, r)
	case apiConfigGet:
		s.handleGetConfig(w, r)
	case apiConfigSchema:
		s.handleConfigMeta(w, r)
	case apiConfigUpdate:
		s.handlePostConfig(w, r)
	case apiConfigPlan:
		s.handlePlanConfig(w, r)
	case apiConfigLocale:
		s.handlePutLocale(w, r)
	case apiConfigTest:
		s.handleConfigTest(w, r)
	case apiConversationReset:
		s.handleConversationReset(w, r)
	case apiMemoryReset:
		s.handleMemoryReset(w, r)
	case apiModels:
		s.handleModels(w, r)
	case apiSTTTestStart:
		s.sttTest.HandleStart(w, r)
	case apiSTTTestStop:
		s.sttTest.HandleStop(w, r)
	case apiStorageStatus:
		s.handleStorageStatus(w, r)
	case apiStorageFormat:
		s.handleStorageFormat(w, r)
	case apiStorageEject:
		s.handleStorageEject(w, r)
	case apiConfigBackupExport:
		s.handleConfigBackupExport(w, r)
	case apiConfigBackupImport:
		s.handleConfigBackupImport(w, r)
	case apiWiFiScan:
		s.handleWiFiScan(w, r)
	case apiWiFiConnect:
		s.handleWiFiConnect(w, r)
	case apiWiFiConnectStatus:
		s.handleWiFiConnectStatus(w, r)
	case apiWiFiForget:
		s.handleWiFiForget(w, r)
	case apiWiFiRegionGet:
		s.handleWiFiRegionGet(w, r)
	case apiWiFiRegionResolve:
		s.handleWiFiRegionResolve(w, r)
	case apiWiFiRegionUpdate:
		s.handleWiFiRegionUpdate(w, r)
	case apiSystemEnvironmentGet:
		s.handleGetSystemEnv(w, r)
	case apiSystemEnvironmentPut:
		s.handleSystemEnv(w, r)
	case apiSystemEnvironmentApply:
		s.handleApplySystemEnv(w, r)
	case apiDeviceStatus:
		s.handleAgentStatus(w, r)
	case apiAgentLogs:
		s.handleAgentLogs(w, r)
	case apiOTAStatus:
		s.handleOTALogs(w, r)
	case apiOTAUpdate:
		s.handleOTAUpdate(w, r)
	case apiDeviceReboot:
		s.handleReboot(w, r)
	case apiAgentRestart:
		s.handleAgentRestart(w, r)
	case apiUSBReenumerate:
		s.handleUSBReenumerate(w, r)
	case apiSupportArchive:
		s.handleSupportLogsExport(w, r)
	case apiLLMLogs:
		s.handleLLMLogs(w, r)
	case apiLLMLogExport:
		s.handleLLMLogExportName(w, match.suffix)
	case apiLLMLogImport:
		s.handleLLMLogImportName(w, r, match.suffix)
	case apiMaintenanceSession:
		s.handleMaintenanceSession(w, r)
	case apiMaintenanceCurrent:
		s.handleMaintenanceCurrent(w, r)
	case apiBackupCapabilities:
		s.handleBackupCapabilities(w, r)
	case apiBackupJobs:
		s.handleCreateBackupJob(w, r)
	case apiBackupJob:
		s.handleBackupJob(w, r, match.suffix)
	case apiBackupArchive:
		s.handleBackupArchive(w, r, match.suffix)
	case apiRestoreJobs:
		s.handleCreateRestoreJob(w, r)
	case apiRestoreJob:
		s.handleRestoreJob(w, r, match.suffix)
	case apiRestoreChunk:
		s.handleRestoreChunk(w, r, match.suffix)
	case apiRestorePlan:
		s.handleRestorePlan(w, r, match.suffix)
	case apiRestoreValidate:
		s.handleRestoreValidate(w, r, match.suffix)
	case apiRestoreApply:
		s.handleRestoreApply(w, r, match.suffix)
	}
}

func endpointConflictsWithMaintenance(endpoint apiEndpoint) bool {
	switch endpoint {
	case apiConfigUpdate, apiConfigLocale, apiConfigTest, apiMemoryReset, apiConversationReset,
		apiSTTTestStart, apiSTTTestStop, apiStorageFormat, apiStorageEject,
		apiConfigBackupImport, apiWiFiScan, apiWiFiConnect, apiWiFiForget,
		apiWiFiRegionUpdate,
		apiSystemEnvironmentPut, apiSystemEnvironmentApply, apiOTAUpdate,
		apiDeviceReboot, apiAgentRestart, apiUSBReenumerate, apiLLMLogImport:
		return true
	default:
		return false
	}
}
