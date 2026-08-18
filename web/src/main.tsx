import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';

import App from './App';
import './index.css';

const container = document.getElementById('root');
if (!container) {
  // The shell is served from the Go binary, so a missing root means the
  // embedded index.html and this bundle have drifted apart.
  throw new Error('LoadWave: #root is missing from the page shell');
}

createRoot(container).render(
  <StrictMode>
    <App />
  </StrictMode>,
);
