async (page) => {
  // CLI-only setup: run in a new nonpersistent session before visiting any app.
  // Playwright CLI's runner has no global URL constructor. Match canonical
  // request URLs with an exact authority boundary, never a loose host prefix.
  const allowed = /^http:\/\/127\.0\.0\.1:(?:18091|18092)(?:\/|$)/;
  const context = page.context();
  const report = { allowed: 0, blocked: 0, websockets: 0 };
  page.__legacyPreviewNetwork = report;
  await context.unroute('**/*'); // This dedicated session has only our routes.
  await context.route('**/*', async (route) => {
    if (!allowed.test(route.request().url())) {
      report.blocked++;
      await route.abort('blockedbyclient');
      return;
    }
    report.allowed++;
    await route.continue();
  });
  await context.routeWebSocket('**/*', (socket) => {
    report.websockets++;
    socket.close({ code: 1008, reason: 'Local preview has no WebSocket backend' });
  });
  context.on('page', (popup) => { if (popup !== page) void popup.close(); });
  await page.goto('http://127.0.0.1:18091/', { waitUntil: 'domcontentloaded' });
  return { url: page.url(), networkGuardInstalled: true };
}
