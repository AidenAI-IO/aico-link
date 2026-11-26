import { Icon } from './ui';
const marks: Record<string, { file: string; dark?: boolean; full?: boolean }> = {
  openai: { file: 'openai', dark: true },
  anthropic: { file: 'anthropic', dark: false },
  openrouter: { file: 'openrouter', dark: true },
  kimi: { file: 'kimi', dark: true },
  'kimi-cn': { file: 'kimi', dark: true },
  volcengine: { file: 'volcengine', dark: false },
  deepseek: { file: 'deepseek', dark: false },
  ollama: { file: 'ollama', dark: true },
  gemini: { file: 'gemini', dark: false },
  // Voice provider types (speech-to-text, text-to-speech, realtime).
  'openai-whisper': { file: 'openai', dark: true },
  qwen: { file: 'qwen', dark: false },
  'qwen-asr': { file: 'qwen', dark: false },
  alicloud: { file: 'alibabacloud', dark: false },
  minimax: { file: 'minimax', dark: false },
  'minimax-cn': { file: 'minimax', dark: false },
  'tencent-asr': { file: 'tencentcloud', dark: false },
  'google-cloud': { file: 'google', dark: false },
  'fish-audio': { file: 'fishaudio', dark: true },
  xai: { file: 'xai', dark: true },
  speko: { file: 'speko', full: true },
  moss: { file: 'moss', full: true },
};
export function ProviderMark({ type, tile = false }: { type: string; tile?: boolean }) {
  const mark = marks[type];
  const size = tile ? 'ds-mark--tile' : 'ds-mark--row';
  if (type === 'custom')
    return (
      <span className={`ds-mark ${size} ds-mark--others`} aria-hidden="true">
        <span className="ds-mark__dots">•••</span>
      </span>
    );
  if (!mark)
    return (
      <span className={`ds-mark ${size} ds-mark--initial`} aria-hidden="true">
        <span className="ds-mark__initial">{(type || '?')[0].toUpperCase()}</span>
      </span>
    );
  return (
    <span
      className={`ds-mark ${size} ${mark.full ? 'ds-mark--full' : mark.dark ? 'ds-mark--dark' : 'ds-mark--light'}`}
      aria-hidden="true"
    >
      <img
        className={`ds-mark__img ${mark.full ? 'ds-mark__img--full' : mark.dark ? 'ds-mark__img--inverted' : ''}`}
        src={`/assets/icons/model/${mark.file}.svg`}
        alt=""
      />
    </span>
  );
}
export function IntroTile({ kind }: { kind: 'wifi' | 'language' | 'search' }) {
  return (
    <svg
      viewBox="0 0 46 46"
      width="46"
      height="46"
      className="ds-tile"
      fill="none"
      aria-hidden="true"
    >
      <path
        d="M31.625 0H14.375C6.43591 0 0 6.43591 0 14.375V31.625C0 39.5641 6.43591 46 14.375 46H31.625C39.5641 46 46 39.5641 46 31.625V14.375C46 6.43591 39.5641 0 31.625 0Z"
        fill="currentColor"
      />
      {kind === 'language' ? (
        <g
          className="ds-tile__globe-stroke"
          stroke="currentColor"
          strokeWidth="1.5"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <circle cx="23" cy="23" r="14.5" />
          <path d="M8.5 23H37.5" />
          <path d="M23 8.5C18.4 12.4 16 17.2 16 23C16 28.8 18.4 33.6 23 37.5C27.6 33.6 30 28.8 30 23C30 17.2 27.6 12.4 23 8.5Z" />
          <path d="M11.5 15.5C14.8 18 18.7 19.2 23 19.2C27.3 19.2 31.2 18 34.5 15.5" />
          <path d="M11.5 30.5C14.8 28 18.7 26.8 23 26.8C27.3 26.8 31.2 28 34.5 30.5" />
        </g>
      ) : kind === 'wifi' ? (
        <path
          className="ds-tile__glyph-fill"
          transform="translate(9.5 14)"
          d="M13.5 5.61225C16.9845 5.61225 20.3265 6.91725 22.7903 9.24075L21.0457 10.8855C16.878 6.95625 10.122 6.95625 5.95425 10.8855L4.209 9.24C6.67275 6.9165 10.0148 5.6115 13.5 5.6115V5.61225ZM27 5.27175L25.2548 6.91725C22.1378 3.97725 17.9085 2.32575 13.5 2.32575C9.09 2.32575 4.86225 3.97725 1.7445 6.91725L0 5.27175C7.45575 -1.75725 19.5443 -1.75725 27 5.27175ZM18.4275 13.3545C15.7058 10.7888 11.2943 10.7888 8.5725 13.3545L13.5 18L18.4275 13.3545Z"
        />
      ) : (
        <g
          className="ds-tile__glyph-stroke"
          transform="translate(12 12) scale(0.9167)"
          strokeWidth="2.2"
          strokeLinecap="round"
          strokeLinejoin="round"
        >
          <path d="M3 10a7 7 0 1 0 14 0a7 7 0 1 0 -14 0M21 21l-6 -6" />
        </g>
      )}
    </svg>
  );
}
export function SelectedCheck() {
  return <Icon name="check" className="ds-choice-row__check" size={24} />;
}
