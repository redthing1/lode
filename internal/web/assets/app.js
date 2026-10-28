"use strict";

async function postJSON(url, body = {}) {
  const response = await fetch(url, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  if (!response.ok) throw new Error(await response.text());
  return response.json();
}
function decode(value) {
  const raw = atob(value.replace(/-/g, "+").replace(/_/g, "/"));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0)).buffer;
}

function encode(value) {
  return btoa(String.fromCharCode(...new Uint8Array(value)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=/g, "");
}

function credentialOptions(options, register) {
  const p = options.publicKey;
  p.challenge = decode(p.challenge);
  if (register) {
    p.user.id = decode(p.user.id);
    if (p.excludeCredentials)
      p.excludeCredentials.forEach((c) => (c.id = decode(c.id)));
  } else if (p.allowCredentials) {
    p.allowCredentials.forEach((c) => (c.id = decode(c.id)));
  }
  return { publicKey: p };
}

function credentialJSON(credential) {
  if (typeof credential.toJSON === "function") return credential.toJSON();
  const response = {};
  for (const key of [
    "clientDataJSON",
    "attestationObject",
    "authenticatorData",
    "signature",
    "userHandle",
  ]) {
    if (credential.response[key])
      response[key] = encode(credential.response[key]);
  }
  if (credential.response.getTransports)
    response.transports = credential.response.getTransports();
  return {
    id: credential.id,
    rawId: encode(credential.rawId),
    type: credential.type,
    response,
    clientExtensionResults: credential.getClientExtensionResults(),
  };
}

for (const button of document.querySelectorAll("[data-security]")) {
  button.addEventListener("click", async () => {
    const purpose = button.dataset.security;
    const register = purpose === "register";
    const status = document.querySelector("[data-security-status]");
    const name = register
      ? document.querySelector("#key-name").value.trim()
      : "";
    if (register && !name) {
      status.textContent = "Give this key a name first.";
      document.querySelector("#key-name").focus();
      return;
    }
    if (!window.PublicKeyCredential) {
      status.textContent =
        "This browser cannot use security keys here. Use a supported browser on the configured HTTPS address.";
      return;
    }
    button.disabled = true;
    status.textContent = "Follow your browser's security key prompt…";
    try {
      const route =
        purpose === "passkey" ? "/passkey/" : "/security/" + purpose + "/";
      const mode =
        register && document.querySelector("#key-mode").value === "key"
          ? "?mode=username"
          : "";
      const body =
        purpose === "passkey"
          ? { username: document.querySelector("#login-username").value.trim() }
          : {};
      const begin = await postJSON(route + "begin" + mode, body);
      const options = credentialOptions(begin.options, register);
      const credential =
        await navigator.credentials[register ? "create" : "get"](options);
      const query = new URLSearchParams({ id: begin.id, name });
      const result = await postJSON(
        route + "finish?" + query,
        credentialJSON(credential),
      );
      if (result.codes && result.codes.length) {
        document.querySelector("[data-recovery-codes]").textContent =
          result.codes.join("\n");
        document.querySelector("[data-recovery-panel]").hidden = false;
        status.textContent =
          "Security key added. Save your recovery codes below.";
        document
          .querySelector("[data-recovery-panel]")
          .scrollIntoView({ block: "nearest" });
      } else {
        window.location.assign(register ? result.next : "/");
      }
    } catch (error) {
      status.textContent =
        error.name === "NotAllowedError"
          ? "The security key request was cancelled or timed out. You can try again."
          : error.message ||
            "The security key could not be verified. Try again.";
    } finally {
      button.disabled = false;
    }
  });
}

for (const select of document.querySelectorAll("[data-auto-submit]")) {
  select.addEventListener("change", () => select.form.requestSubmit());
}

for (const button of document.querySelectorAll("[data-copy]")) {
  button.addEventListener("click", async () => {
    const panel = button.parentElement;
    const status = panel.querySelector("[data-copy-status]");
    try {
      await navigator.clipboard.writeText(
        panel.querySelector("[data-copy-value]").textContent,
      );
      status.textContent = "Copied.";
    } catch {
      status.textContent = "Select the value and copy it manually.";
    }
  });
}
