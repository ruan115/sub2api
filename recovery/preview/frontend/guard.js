/* Local visual preview only. The server and CSP enforce the same boundary. */
(() => {
  "use strict";
  const localOrigin = location.origin;
  const reject = () => new DOMException("本地预览：写操作和外部连接已禁用", "SecurityError");
  function permitted(value, method = "GET") {
    try {
      const url = new URL(value, location.href);
      return url.origin === localOrigin && ["GET", "HEAD"].includes(String(method).toUpperCase());
    } catch { return false; }
  }
  // This origin is dedicated to a new local preview, not any production user.
  // Keep the user's selected theme; override only these preview-owned values.
  localStorage.setItem("portunex_token", "sess_LOCAL_PREVIEW_SYNTHETIC_NOT_A_CREDENTIAL");
  localStorage.setItem("portunex_window_summary_refresh_interval", "0");
  const originalFetch = window.fetch.bind(window);
  window.fetch = (input, init) => {
    const request = input instanceof Request ? input : null;
    if (!permitted(request ? request.url : input, init?.method || request?.method || "GET")) {
      return Promise.reject(reject());
    }
    return originalFetch(input, { ...init, credentials: "omit", referrerPolicy: "no-referrer" });
  };
  const open = XMLHttpRequest.prototype.open;
  XMLHttpRequest.prototype.open = function (method, url, ...rest) {
    if (!permitted(url, method)) throw reject();
    return open.call(this, method, url, ...rest);
  };
  window.open = () => { throw reject(); };
  window.WebSocket = class { constructor() { throw reject(); } };
  window.EventSource = class { constructor() { throw reject(); } };
  navigator.sendBeacon = () => false;
  document.addEventListener("submit", event => {
    event.preventDefault();
    event.stopImmediatePropagation();
  }, true);
  document.addEventListener("click", event => {
    const link = event.target instanceof Element ? event.target.closest("a") : null;
    if (link && (link.hasAttribute("download") || !permitted(link.href))) {
      event.preventDefault();
      event.stopImmediatePropagation();
    }
  }, true);
})();
