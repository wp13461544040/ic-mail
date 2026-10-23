// A same-origin recovery UI remains usable when the application module fails.
(function () {
  var timer;
  var observer;
  function pending() { return document.getElementById("startup-message"); }
  function showFailure() {
    var message = pending();
    if (!message) {
      var banner = document.getElementById("resource-recovery");
      if (banner) banner.hidden = false;
      return;
    }
    message.textContent = "页面资源加载较慢或连接中断，请重新加载。";
    document.getElementById("startup-reload").hidden = false;
  }
  function cleanup() {
    clearTimeout(timer);
    if (observer) observer.disconnect();
  }
  function resourceError(event) {
    if (event.target && (event.target.tagName === "SCRIPT" || event.target.tagName === "LINK")) showFailure();
  }
  window.addEventListener("error", resourceError, true);
  window.addEventListener("vite:preloadError", showFailure);
  window.addEventListener("icloud:startup-error", showFailure);
  function start() {
    var reload = document.getElementById("startup-reload");
    var resourceReload = document.getElementById("resource-reload");
    function refresh() { window.location.reload(); }
    if (reload) reload.addEventListener("click", refresh);
    if (resourceReload) resourceReload.addEventListener("click", refresh);
    if (pending()) {
      timer = setTimeout(showFailure, 15000);
      observer = new MutationObserver(function () { if (!pending()) cleanup(); });
      observer.observe(document.getElementById("app"), { childList: true, subtree: true });
    }
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", start, { once: true });
  else start();
})();
