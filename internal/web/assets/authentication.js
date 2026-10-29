"use strict";

const decodeCredentialBytes = value => Uint8Array.from(atob(value.replace(/-/g, "+").replace(/_/g, "/")), c => c.charCodeAt(0));
const encodeCredentialBytes = value => btoa(Array.from(new Uint8Array(value), byte => String.fromCharCode(byte)).join("")).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");

function credentialJSON(credential) {
  const source = credential.response;
  const response = {clientDataJSON: encodeCredentialBytes(source.clientDataJSON)};
  if (source.attestationObject) {
    response.attestationObject = encodeCredentialBytes(source.attestationObject);
    response.transports = source.getTransports ? source.getTransports() : [];
  } else {
    response.authenticatorData = encodeCredentialBytes(source.authenticatorData);
    response.signature = encodeCredentialBytes(source.signature);
    response.userHandle = source.userHandle ? encodeCredentialBytes(source.userHandle) : null;
  }
  return {id: credential.id, rawId: encodeCredentialBytes(credential.rawId), type: credential.type,
    response, clientExtensionResults: credential.getClientExtensionResults(), authenticatorAttachment: credential.authenticatorAttachment};
}

async function keyRequest(url, body, json = false) {
  const response = await fetch(url, {method: "POST", credentials: "same-origin",
    headers: json ? {"Content-Type": "application/json"} : {}, body});
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || "The key request failed.");
  return result;
}

for (const form of document.querySelectorAll("form[data-webauthn]")) {
  form.addEventListener("submit", async event => {
    event.preventDefault();
    const button = form.querySelector("button"), error = form.querySelector(".key-error");
    button.disabled = true;
    error.hidden = true;
    try {
      if (!window.PublicKeyCredential || !navigator.credentials) throw new Error("This browser cannot use security keys here. Use HTTPS or localhost.");
      const ceremony = await keyRequest(form.action, new URLSearchParams(new FormData(form)));
      const options = ceremony.options, publicKey = options.publicKey;
      publicKey.challenge = decodeCredentialBytes(publicKey.challenge);
      if (publicKey.user) publicKey.user.id = decodeCredentialBytes(publicKey.user.id);
      for (const descriptor of [...(publicKey.excludeCredentials || []), ...(publicKey.allowCredentials || [])]) descriptor.id = decodeCredentialBytes(descriptor.id);
      const credential = form.dataset.webauthn === "create" ? await navigator.credentials.create(options) : await navigator.credentials.get(options);
      if (!credential) throw new Error("No key was selected.");
      const result = await keyRequest(form.action + "/finish?id=" + encodeURIComponent(ceremony.id), JSON.stringify(credentialJSON(credential)), true);
      if (result.codes && result.codes.length) {
        const panel = document.querySelector("#key-recovery");
        panel.querySelector("pre").textContent = result.codes.join("\n");
        panel.hidden = false;
        document.querySelector("#security-settings").hidden = true;
        panel.scrollIntoView({block: "start"});
      } else {
        window.location.assign(result.next);
      }
    } catch (failure) {
      error.textContent = failure.name === "NotAllowedError" ? "The key request was cancelled or unavailable." : failure.message;
      error.hidden = false;
    } finally {
      button.disabled = false;
      const password = form.querySelector('input[type="password"]');
      if (password) password.value = "";
    }
  });
}
