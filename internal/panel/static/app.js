"use strict";
(function () {
  var $ = function (id) { return document.getElementById(id); };
  var views = ["loading", "signedin", "login", "setup"];

  function show(name) {
    views.forEach(function (v) { $(v).hidden = v !== name; });
  }
  function msg(text) {
    $("msg").textContent = text || "";
    $("msg").hidden = !text;
  }
  function post(path, body) {
    return fetch(path, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {})
    }).then(function (res) {
      if (res.status === 204) { return {}; }
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          var e = new Error(data.error || "request failed");
          e.retryAfter = res.headers.get("Retry-After");
          throw e;
        }
        return data;
      });
    });
  }
  function fail(err) {
    var t = err.message;
    if (err.retryAfter) { t += " (retry in " + err.retryAfter + "s)"; }
    msg(t);
  }
  function busy(form, on) {
    Array.prototype.forEach.call(form.querySelectorAll("button"), function (b) { b.disabled = on; });
  }
  function signedIn(u) {
    msg("");
    $("who-name").textContent = u.username;
    $("who-role").textContent = u.role;
    show("signedin");
  }

  var token = "";
  if (location.pathname === "/setup" && location.hash.length > 1) {
    token = decodeURIComponent(location.hash.slice(1));
  }

  $("login").addEventListener("submit", function (ev) {
    ev.preventDefault();
    msg("");
    busy($("login"), true);
    post("/auth/login", { username: $("l-user").value, password: $("l-pass").value, code: $("l-code").value })
      .then(function (u) { $("l-pass").value = ""; $("l-code").value = ""; signedIn(u); })
      .catch(fail)
      .then(function () { busy($("login"), false); });
  });

  $("setup").addEventListener("submit", function (ev) {
    ev.preventDefault();
    msg("");
    if ($("s-pass").value !== $("s-pass2").value) { msg("Passwords do not match."); return; }
    busy($("setup"), true);
    post("/auth/setup/finish", { token: token, password: $("s-pass").value, code: $("s-code").value })
      .then(function (u) {
        $("s-pass").value = ""; $("s-pass2").value = ""; $("s-code").value = "";
        history.replaceState(null, "", "/");
        token = "";
        signedIn(u);
      })
      .catch(fail)
      .then(function () { busy($("setup"), false); });
  });

  $("logout").addEventListener("click", function () {
    post("/auth/logout").catch(function () {}).then(function () { msg(""); show("login"); });
  });

  fetch("/auth/session", { credentials: "same-origin" })
    .then(function (res) { return res.ok ? res.json() : null; })
    .catch(function () { return null; })
    .then(function (u) {
      if (u) { signedIn(u); return; }
      if (token) {
        return post("/auth/setup/begin", { token: token }).then(function (info) {
          $("s-user").textContent = info.username;
          $("s-role").textContent = info.role;
          $("s-secret").textContent = info.totp_secret;
          $("s-uri").textContent = info.otpauth_uri;
          show("setup");
        }).catch(function (err) { msg(err.message); show("login"); });
      }
      show("login");
    });
})();
