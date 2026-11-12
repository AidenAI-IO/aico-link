export const runtime = Object.create(null);
export const appState = {config:null,wifi:null,wifiStatus:null,wifiRegion:null,agentStatus:null,agentLog:null,agentLogPendingSnapshot:null,agentLogFailureView:null,agentLogBackgroundError:null,agentLogAutoScroll:true,otaLog:null,otaLogVisible:false,otaLogPending:false,otaLogStartSize:0,otaUpdateRunning:false,systemEnv:'',networks:[],selectedSsid:'',wifiListExpanded:false,sttTest:{recording:false,busy:false},testToast:{owner:null,generation:0,view:null}};
export const sectionSnapshots = {};
export const modelProvidersByName = {};

export function byId(id) { return document.getElementById(id); }
export function configureTerminalLink(port = 8080) {
  const link = byId('terminalLink');
  if (!link) return;
  const terminalUrl = new URL(window.location.href);
  terminalUrl.port = terminalUrl.hostname === '192.168.42.1' ? '3000' : String(port || 8080);
  terminalUrl.pathname = '/webtty/';
  terminalUrl.search = '';
  terminalUrl.hash = '';
  link.href = terminalUrl.toString();
}
export function registerRuntime(bindings) { Object.assign(runtime, bindings); }
export function runtimeFunction(name) { return (...args) => runtime[name](...args); }
export function runtimeObject(name) {
  const target = () => typeof runtime[name] === 'function' ? runtime[name]() : runtime[name];
  return new Proxy({}, {
    get(_target, key) { return target()[key]; },
    set(_target, key, value) { target()[key] = value; return true; },
    ownKeys() { return Reflect.ownKeys(target()); },
    getOwnPropertyDescriptor(_target, key) {
      const descriptor = Object.getOwnPropertyDescriptor(target(), key);
      return descriptor ? {...descriptor, configurable: true} : undefined;
    }
  });
}

registerRuntime({appState,sectionSnapshots,modelProvidersByName,byId,configureTerminalLink});
