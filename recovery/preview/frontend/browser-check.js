async (page) => {
  // Read-only QA for the dedicated default-port session after opening dashboard.
  const frame = page.frames().find(frame => frame.url() === 'http://127.0.0.1:18092/dashboard');
  if (!frame || !page.__legacyPreviewNetwork) throw new Error('Dedicated preview session required');
  const checks = await frame.evaluate(async () => {
    const rejected = async (operation) => {
      try { await operation(); return false; }
      catch (error) { return error.name === 'SecurityError'; }
    };
    return {
      dashboardVisible: document.body.innerText.includes('总请求数'),
      syntheticUser: document.body.innerText.includes('preview@example.invalid'),
      theme: document.documentElement.classList.contains('dark') ? 'dark' : 'light',
      externalFetchRejected: await rejected(() => fetch('https://preview-probe.invalid/')),
      writeFetchRejected: await rejected(() => fetch('/portunex/admin/providers', {method: 'POST'})),
      webSocketRejected: await rejected(() => new WebSocket('ws://127.0.0.1:18092/')),
      popupRejected: await rejected(() => window.open('https://preview-probe.invalid/')),
      beaconRejected: navigator.sendBeacon('/portunex/admin/providers', '') === false,
      resourceOrigins: [...new Set(performance.getEntriesByType('resource')
        .filter(entry => /^https?:/.test(entry.name)).map(entry => new URL(entry.name).origin))],
    };
  });
  // iframe restrictions are inspected in its parent, never guessed from the child.
  checks.iframeSandbox = await page.locator('iframe').getAttribute('sandbox');
  if (checks.iframeSandbox !== 'allow-scripts allow-same-origin') throw new Error('Unexpected iframe sandbox');
  for (const key of ['dashboardVisible', 'syntheticUser', 'externalFetchRejected', 'writeFetchRejected',
                     'webSocketRejected', 'popupRejected', 'beaconRejected']) {
    if (!checks[key]) throw new Error('Preview QA failed: ' + key);
  }
  if (checks.resourceOrigins.some(origin => origin !== 'http://127.0.0.1:18092')) {
    throw new Error('Unexpected resource origin');
  }
  return {checks, network: page.__legacyPreviewNetwork};
}
