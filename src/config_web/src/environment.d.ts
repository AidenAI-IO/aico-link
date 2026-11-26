interface Window {
  ReactNativeWebView?: { postMessage(message: string): void };
  webkit?: { messageHandlers?: Record<string, unknown> };
}
