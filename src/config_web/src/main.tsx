import { createRoot } from 'react-dom/client';
import { Provider } from 'jotai';
import { request } from './api/client';
import { App } from './app/App';
import { createDeviceState } from './state/device';

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
