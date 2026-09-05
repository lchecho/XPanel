// 前端组件：HTMX 渐进增强配置。核心流程不依赖本脚本；脚本只负责局部刷新时的焦点保持与失败提示。
(function () {
  "use strict";
  var focusedId = null;
  var status = document.getElementById("refresh-status");

  document.addEventListener("htmx:configRequest", function () {
    var active = document.activeElement;
    focusedId = active && active.id ? active.id : null;
  });

  document.addEventListener("htmx:afterSwap", function (event) {
    if (focusedId) {
      var restored = document.getElementById(focusedId);
      if (restored) {
        restored.focus({ preventScroll: true });
      } else {
        var summary = event.target.querySelector("caption, h2, .empty");
        if (summary) {
          summary.setAttribute("tabindex", "-1");
          summary.focus({ preventScroll: true });
        }
      }
    }
    if (status) {
      status.textContent = "已于 " + new Date().toLocaleTimeString() + " 刷新。";
      status.classList.remove("stale");
    }
  });

  function markStale(detail) {
    if (!status) {
      return;
    }
    status.textContent = "自动刷新失败（" + detail + "），页面显示的是最后一次确认的数据。";
    status.classList.add("stale");
  }

  document.addEventListener("htmx:responseError", function (event) {
    markStale("HTTP " + (event.detail && event.detail.xhr ? event.detail.xhr.status : "错误"));
  });
  document.addEventListener("htmx:sendError", function () { markStale("网络错误"); });
  document.addEventListener("htmx:timeout", function () { markStale("超时"); });

  // 会话失效时 fragment 返回 403：停止轮询并提示重新登录，避免把登录页换进表格。
  document.addEventListener("htmx:beforeSwap", function (event) {
    if (event.detail && event.detail.xhr && event.detail.xhr.status === 403) {
      event.detail.shouldSwap = false;
      markStale("会话已失效，请重新登录");
    }
    if (event.detail && event.detail.xhr && event.detail.xhr.status === 503) {
      event.detail.shouldSwap = false;
      markStale("服务暂时不可用");
    }
  });
})();
