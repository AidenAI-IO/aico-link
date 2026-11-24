import { createRoot } from 'react-dom/client';
import { Provider } from 'jotai';
import { request } from '../assets/js/config/api.js';
import { App } from './app';
import { createDeviceState } from './state';

const model = createDeviceState(request, {
  local: document.documentElement.dataset.profile === 'local',
  container: document.documentElement.dataset.profile === 'container',
});
const root = createRoot(document.getElementById('app')!);
root.render(
  <Provider store={model.store}>
    <App model={model} />
  </Provider>,
);
