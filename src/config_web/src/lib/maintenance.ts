// Request IDs are UUIDs even on HTTP, where randomUUID is unavailable.
export function createMaintenanceRequestID(
  cryptoAPI: Partial<Pick<Crypto, 'randomUUID' | 'getRandomValues'>> = globalThis.crypto,
) {
  if (typeof cryptoAPI?.randomUUID === 'function') return cryptoAPI.randomUUID();
  const bytes = new Uint8Array(16);
  if (typeof cryptoAPI?.getRandomValues === 'function') cryptoAPI.getRandomValues(bytes);
  else
    bytes.forEach((_value, index) => {
      bytes[index] = Math.floor(Math.random() * 256);
    });
  bytes[6] = (bytes[6] & 0x0f) | 0x40;
  bytes[8] = (bytes[8] & 0x3f) | 0x80;
  const hex = Array.from(bytes, (value) => value.toString(16).padStart(2, '0')).join('');
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
