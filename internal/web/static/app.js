// Progressive enhancement for the server-rendered interface. Every core flow remains usable without JavaScript.
(function () {
  "use strict";

  var focusedId = null;

  function refreshStatus() {
    return document.getElementById("refresh-status");
  }

  function setRefreshMessage(message, stale) {
    var status = refreshStatus();
    if (!status) return;
    status.textContent = message;
    status.classList.toggle("stale", Boolean(stale));
  }

  document.addEventListener("htmx:configRequest", function () {
    var active = document.activeElement;
    focusedId = active && active.id ? active.id : null;
    setRefreshMessage("正在获取最新状态…", false);
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
    setRefreshMessage("刚刚完成自动更新 · " + new Date().toLocaleTimeString(), false);
  });

  function markStale(detail) {
    setRefreshMessage("自动更新失败（" + detail + "）。当前保留最后一次确认的数据，请检查连接后重试。", true);
  }

  document.addEventListener("htmx:responseError", function (event) {
    markStale("HTTP " + (event.detail && event.detail.xhr ? event.detail.xhr.status : "错误"));
  });
  document.addEventListener("htmx:sendError", function () { markStale("网络错误"); });
  document.addEventListener("htmx:timeout", function () { markStale("请求超时"); });

  // A fragment must never replace the table with a login or generic error page.
  document.addEventListener("htmx:beforeSwap", function (event) {
    if (!event.detail || !event.detail.xhr) return;
    if (event.detail.xhr.status === 403) {
      event.detail.shouldSwap = false;
      markStale("会话已失效，请重新登录");
    }
    if (event.detail.xhr.status === 503) {
      event.detail.shouldSwap = false;
      markStale("服务暂时不可用");
    }
  });

  function resetSubmitting(form) {
    form.removeAttribute("aria-busy");
    form.removeAttribute("data-submitting");
    form.querySelectorAll("button[type='submit']").forEach(function (button) {
      button.disabled = false;
      if (button.dataset.originalLabel) {
        button.textContent = button.dataset.originalLabel;
        delete button.dataset.originalLabel;
      }
    });
  }

  document.addEventListener("submit", function (event) {
    var form = event.target;
    if (!(form instanceof HTMLFormElement)) return;
    if (form.hasAttribute("data-submitting")) {
      event.preventDefault();
      return;
    }
    form.setAttribute("data-submitting", "true");
    form.setAttribute("aria-busy", "true");
    // Defer disabling until the browser has captured the successful submitter.
    window.setTimeout(function () {
      form.querySelectorAll("button[type='submit']").forEach(function (button) {
        button.dataset.originalLabel = button.textContent;
        button.textContent = form.dataset.submitLabel || "正在提交…";
        button.disabled = true;
      });
    }, 0);
  });

  window.addEventListener("pageshow", function () {
    document.querySelectorAll("form[data-submitting]").forEach(resetSubmitting);
  });

  function syncUnlimited(toggle) {
    var targetId = toggle.getAttribute("aria-controls");
    var target = targetId ? document.getElementById(targetId) : null;
    if (!target) return;
    target.querySelectorAll("input, select").forEach(function (control) {
      control.disabled = toggle.checked;
    });
    target.setAttribute("aria-disabled", toggle.checked ? "true" : "false");
  }

  document.querySelectorAll("[data-unlimited-toggle]").forEach(function (toggle) {
    syncUnlimited(toggle);
    toggle.addEventListener("change", function () { syncUnlimited(toggle); });
  });

  function fallbackCopy(text) {
    var temporary = document.createElement("textarea");
    temporary.value = text;
    temporary.setAttribute("readonly", "");
    temporary.className = "sr-only";
    document.body.appendChild(temporary);
    temporary.select();
    var copied = document.execCommand("copy");
    temporary.remove();
    return copied ? Promise.resolve() : Promise.reject(new Error("copy failed"));
  }

  document.addEventListener("click", function (event) {
    var copyButton = event.target.closest("[data-copy-target]");
    if (copyButton) {
      var target = document.getElementById(copyButton.dataset.copyTarget);
      var feedback = document.getElementById("copy-feedback");
      if (!target) return;
      var value = "value" in target ? target.value : target.textContent;
      var copy = navigator.clipboard && window.isSecureContext ? navigator.clipboard.writeText(value) : fallbackCopy(value);
      copy.then(function () {
        var original = copyButton.textContent;
        copyButton.textContent = "已复制";
        if (feedback) feedback.textContent = (copyButton.dataset.copyLabel || "内容") + "已复制到剪贴板。";
        window.setTimeout(function () { copyButton.textContent = original; }, 1800);
      }).catch(function () {
        if (feedback) feedback.textContent = "自动复制失败，请选中内容后手动复制。";
        if (target.select) target.select();
      });
      return;
    }

    var backButton = event.target.closest("[data-history-back]");
    if (backButton) {
      if (window.history.length > 1) window.history.back();
      else window.location.assign("/");
    }
  });

  var errorSummary = document.querySelector(".error-summary");
  if (errorSummary) errorSummary.focus({ preventScroll: true });

  var currentNav = document.querySelector('.primary-nav [aria-current="page"]');
  if (currentNav && window.matchMedia("(max-width: 48rem)").matches) {
    currentNav.scrollIntoView({ inline: "center", block: "nearest" });
  }
})();
