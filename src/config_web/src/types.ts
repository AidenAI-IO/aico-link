import type { ReactNode } from 'react';
import type { DeviceState } from './state/device';

export type TextSpec = string | { key: string; fallback?: string };
export type Revision = string | number;
export interface Configuration {
  agent?: { locale?: string; input_mode?: string; mcp_enabled?: boolean; [key: string]: unknown };
  model?: { provider?: string; model?: string; reasoning_effort?: string; [key: string]: unknown };
  model_providers?: Record<string, ProviderRecord>;
  [section: string]: unknown;
}
export interface WiFiStatus {
  connected: boolean;
  state: string;
  ssid?: string;
  ip_address?: string;
  observed_at_unix_us?: number;
  signal_dbm?: number;
}
export interface WiFiProfile {
  ssid: string;
  proxy_mode?: string;
  proxy_url?: string;
  has_psk?: boolean;
  disabled?: boolean;
  priority?: number;
}
export interface WiFiData {
  wifi_status: WiFiStatus;
  wifi?: { networks?: WiFiProfile[] };
}
export interface Network {
  ssid: string;
  secured: boolean;
  signalDbm?: number | null;
  signalPercent?: number | null;
}
export interface StorageData {
  internal?: { available?: boolean; total_bytes?: number; free_bytes?: number };
  card?: { mounted?: boolean; present?: boolean };
  format_job?: { status?: string; fs?: string; error?: string };
  effective_mode?: number;
  last_warning?: string;
}
export interface Firmware {
  current_version?: string;
  version?: string;
  business_version?: string;
  health_status?: string;
  health_error?: string;
  running_slot?: string;
  current_build_time?: string;
  build_time?: string;
  previous_version?: string;
  components?: { boot?: string; rootfs?: string };
}
export interface AgentStatus {
  process_running?: boolean;
  port_reachable?: boolean;
  device_type?: string;
  url?: string;
  terminal_url?: string;
  classic_voice_enabled?: boolean;
}
export interface DeviceData {
  firmware?: Firmware;
  agent_status?: AgentStatus;
}
export interface Snapshot extends Partial<WiFiData>, DeviceData {
  snapshot_error?: { message: string; code?: string };
  config?: Configuration | null;
  storage?: StorageData;
  config_valid?: boolean;
  config_errors?: { message: string }[];
  paths?: { agent_config?: string };
  profile?: string;
}
export interface ApplicationData {
  state?: string;
  error?: string;
  ok?: boolean;
  pending?: boolean;
  applied?: boolean;
  reboot_required?: boolean;
  revision?: Revision;
  runtime_id?: string;
  config?: Configuration;
  locale?: string;
}
export interface USBData {
  enabled: boolean;
  active?: boolean;
  host?: string;
  via_usb?: boolean;
}
export interface OTAData {
  ota_update_running?: boolean;
  hybrid_update_phase?: string;
  ota_log?: { log?: string };
  ota_health_log?: { log?: string };
}
export interface MCPServer {
  name: string;
  state: string;
  enabled: boolean;
  error?: string;
  tools?: string[];
}
export interface MCPData {
  servers?: MCPServer[];
}
export interface LANData {
  enabled?: boolean;
  password_set?: boolean;
  via_usb?: boolean;
  min_length?: number;
  system_synced?: boolean;
}
export interface MirroringData {
  ok?: boolean;
  supported?: boolean;
  restored?: boolean;
  screenshot_available?: boolean;
  code?: string;
}
export interface OperationResult {
  ok?: boolean;
  warning_code?: string;
  code?: string;
}
export interface HeaderSpec {
  title?: TextSpec;
  onBack?: (() => void) | null;
  action?: ReactNode;
}
export type Navigate = (path: string, options?: { replace?: boolean; silent?: boolean }) => unknown;
export interface Route {
  path: string;
  pattern?: string;
  name?: string;
  params?: Record<string, string>;
  query?: Record<string, string>;
}
export interface PageProps {
  model: DeviceState;
  route: Route;
  navigate: Navigate;
  header: (value: HeaderSpec) => void;
}
export type Transport = (path: string, options?: RequestInit) => Promise<unknown>;

export interface ProviderRecord {
  type: string;
  has_api_key?: boolean;
  base_url?: string;
  [key: string]: unknown;
}
export type JsonValue = string | number | boolean | null | JsonValue[] | JsonObject;
export interface JsonObject {
  [key: string]: JsonValue | undefined;
}
export type ConfigPatch = JsonObject;
