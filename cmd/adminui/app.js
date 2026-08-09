(() => {
  "use strict";

  const state = { csrf: "", providers: [], active: "", vaultAvailable: false, vaultLocked: true, backend: "" };
  const $ = (id) => document.getElementById(id);
  const loginView = $("login-view");
  const providersView = $("providers-view");
  const notice = $("notice");
  const keyDialog = $("key-dialog");
  const confirmDialog = $("confirm-dialog");

  async function api(path, options = {}) {
    const headers = new Headers(options.headers || {});
    if (state.csrf && options.method && options.method !== "GET") headers.set("X-CSRF-Token", state.csrf);
    const response = await fetch(path, { ...options, headers, credentials: "same-origin" });
    let payload = {};
    if ((response.headers.get("content-type") || "").includes("application/json")) payload = await response.json();
    if (!response.ok) {
      const error = new Error(payload.error || `Request failed (${response.status})`);
      error.status = response.status;
      throw error;
    }
    return payload;
  }

  function showNotice(message, isError = false) {
    notice.textContent = message;
    notice.classList.toggle("error", isError);
    notice.hidden = false;
    window.setTimeout(() => { notice.hidden = true; }, 5000);
  }

  function providerLabel(id) {
    return ({ openrouter: "OpenRouter", cerebras: "Cerebras", groq: "Groq" })[id] || id;
  }

  function render() {
    const list = $("provider-list");
    list.replaceChildren();
    for (const provider of state.providers) {
      const card = $("provider-template").content.firstElementChild.cloneNode(true);
      card.dataset.provider = provider.provider;
      card.classList.toggle("configured", provider.configured);
      card.classList.toggle("invalid", provider.status === "unavailable");
      card.querySelector(".provider-icon").textContent = providerLabel(provider.provider).slice(0, 2).toUpperCase();
      card.querySelector(".provider-name").textContent = providerLabel(provider.provider);
      const environment = provider.source === "environment";
	  const vaultUnavailable = state.vaultLocked || !state.vaultAvailable;
      card.querySelector(".provider-detail").textContent = environment ? "Environment override" : (vaultUnavailable ? "Vault locked or unavailable" : (provider.configured ? "Protected value stored" : "No key stored"));
      card.querySelector(".status-text").textContent = environment ? "Environment key is active" : (vaultUnavailable ? "Managed keys unavailable" : (provider.configured ? "Configured in encrypted vault" : "Not configured"));
      const keyButton = card.querySelector(".key-button");
      keyButton.textContent = provider.configured ? "Replace" : "Add key";
	  keyButton.disabled = vaultUnavailable;
      keyButton.addEventListener("click", () => openKeyDialog(provider.provider, provider.configured));
      const testButton = card.querySelector(".test-button");
	  testButton.disabled = !provider.configured && !environment;
      testButton.addEventListener("click", () => testProvider(provider.provider, testButton));
      const deleteButton = card.querySelector(".delete-button");
      deleteButton.hidden = !provider.configured;
	  deleteButton.disabled = vaultUnavailable;
      deleteButton.addEventListener("click", () => openDeleteDialog(provider.provider));
      list.append(card);
    }
    list.setAttribute("aria-busy", "false");
  }

  async function loadProviders() {
    try {
      const payload = await api("/admin/api/providers");
      state.csrf = payload.csrf_token || "";
      state.providers = payload.providers || [];
	  state.vaultAvailable = payload.available === true;
	  state.vaultLocked = payload.locked === true;
	  state.backend = payload.backend || "unknown";
      loginView.hidden = true;
      providersView.hidden = false;
      render();
	  if (state.vaultLocked || !state.vaultAvailable) {
	    showNotice(`Managed-key vault is unavailable (${state.backend}). Environment credentials still work.`, true);
	  }
    } catch (error) {
      if (error.status === 401) {
        providersView.hidden = true;
        loginView.hidden = false;
        $("admin-token").focus();
        return;
      }
      loginView.hidden = true;
      providersView.hidden = false;
      showNotice(error.message, true);
    }
  }

  function openKeyDialog(provider, configured) {
    state.active = provider;
    $("dialog-kicker").textContent = providerLabel(provider);
    $("dialog-title").textContent = configured ? "Replace API key" : "Add API key";
    $("dialog-error").textContent = "";
    $("provider-key").value = "";
    keyDialog.showModal();
    $("provider-key").focus();
  }

  function closeKeyDialog() {
    $("provider-key").value = "";
    keyDialog.close();
  }

  function openDeleteDialog(provider) {
    state.active = provider;
    $("confirm-copy").textContent = `${providerLabel(provider)} will stop being available until a new key is added.`;
    confirmDialog.showModal();
  }

  async function testProvider(provider, button) {
    button.disabled = true;
    button.textContent = "Testing…";
    try {
      const result = await api(`/admin/api/providers/${encodeURIComponent(provider)}/test`, { method: "POST" });
      showNotice(`${providerLabel(provider)} connection succeeded (${result.latency_ms} ms).`);
    } catch (error) {
      showNotice(`${providerLabel(provider)} test failed: ${error.message}`, true);
    } finally {
      button.textContent = "Test";
      button.disabled = false;
    }
  }

  $("login-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const input = $("admin-token");
    $("login-error").textContent = "";
    try {
      const payload = await api("/admin/session", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ token: input.value })
      });
      state.csrf = payload.csrf_token || "";
      input.value = "";
      await loadProviders();
    } catch (error) {
      input.value = "";
      $("login-error").textContent = error.message;
      input.focus();
    }
  });

  $("key-form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const input = $("provider-key");
    const save = $("dialog-save");
    save.disabled = true;
    $("dialog-error").textContent = "";
    try {
      await api(`/admin/api/providers/${encodeURIComponent(state.active)}/key`, {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ key: input.value })
      });
      closeKeyDialog();
      await loadProviders();
      showNotice(`${providerLabel(state.active)} key saved.`);
    } catch (error) {
      input.value = "";
      $("dialog-error").textContent = error.message;
      input.focus();
    } finally {
      save.disabled = false;
    }
  });

  $("confirm-dialog").addEventListener("close", async () => {
    if (confirmDialog.returnValue !== "confirm") return;
    try {
      await api(`/admin/api/providers/${encodeURIComponent(state.active)}/key`, { method: "DELETE" });
      await loadProviders();
      showNotice(`${providerLabel(state.active)} key removed.`);
    } catch (error) { showNotice(error.message, true); }
  });

  $("lock-button").addEventListener("click", async () => {
    try { await api("/admin/session", { method: "DELETE" }); } catch (_) { /* session is locked either way */ }
    state.csrf = "";
    state.providers = [];
	state.vaultAvailable = false;
	state.vaultLocked = true;
    providersView.hidden = true;
    loginView.hidden = false;
    $("admin-token").focus();
  });
  $("dialog-close").addEventListener("click", closeKeyDialog);
  $("dialog-cancel").addEventListener("click", closeKeyDialog);
  $("confirm-cancel").addEventListener("click", () => confirmDialog.close("cancel"));
  $("confirm-delete").addEventListener("click", () => confirmDialog.close("confirm"));
  loadProviders();
})();
