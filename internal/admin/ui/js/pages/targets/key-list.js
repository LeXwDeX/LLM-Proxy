"use strict";
// Targets key card list inside the target modal: add, remove, block, reload and status refresh.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsKeyList", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var maskKey = app.bind("core", "maskKey");
  var esc = app.bind("core", "esc");
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var showConfirm = app.bind("core", "showConfirm");
  var keyCooldownEnd = app.bind("targetsHealth", "keyCooldownEnd");
  var keyListFromTarget = app.bind("targetsHealth", "keyListFromTarget");
  var keyIdsUsable = app.bind("targetsHealth", "keyIdsUsable");
  var keyRuntimeByID = app.bind("targetsHealth", "keyRuntimeByID");
  var targetETag = app.bind("targetsHealth", "targetETag");
  var openTargetModal = app.bind("targetsModal", "openTargetModal");
  var keyStatusColor = app.bind("targetsHealth", "keyStatusColor");
  var loadTargets = app.bind("targets", "loadTargets");
  // Saved keys follow the fresh target by stable ID; their status comes from its runtime map.
  function updateEditingKeyStatuses(t) {
    ts.tgtKeyList.forEach(function (item) {
      if (!item.existing || !item.keyId) return;
      var runtime = keyRuntimeByID(t, item.keyId);
      item.runtimeIndex = runtime.runtimeIndex;
      item.status = runtime.status;
    });
    ts.editingKeyStatusStale = false;
  }
  // Called for every published target list while a target is being edited. Only
  // the edited target counts. A changed ETag is a hint, never a block: the
  // server arbitrates the save through If-Match.
  function syncEditingWithRefresh() {
    var name = state.editingTarget && state.editingTarget.name;
    var fresh = state.targets.find(function (t) { return t.name === name; });
    if (!fresh) {
      ts.editingRemote = "missing";
      return;
    }
    // A save in flight may already have advanced the ETag it is about to report.
    if (!ts.editingKeySaving) ts.editingRemote = targetETag(fresh) !== ts.editingETag ? "changed" : "";
    updateEditingKeyStatuses(fresh);
  }
  async function refreshEditingTarget() {
    var session = ts.editingKeySession;
    var name = state.editingTarget && state.editingTarget.name;
    var targets = await loadTargets();
    if (session !== ts.editingKeySession || !state.editingTarget || state.editingTarget.name !== name) return;
    // On success loadTargets already synced the editor; a failed load keeps the last statuses.
    if (!targets) ts.editingKeyStatusStale = true;
    renderKeyList();
  }

  // Reload the edited target: the whole form is replaced with the server's current
  // content. The draft, including keys typed in but not yet saved, is discarded
  // (the user confirmed that), so nothing stale can overwrite someone else's change.
  async function reloadEditingTarget(session, name) {
    var targets;
    try {
      targets = await loadTargets(true);
    } catch (e) {
      targets = null;
    }
    if (session !== ts.editingKeySession || !state.editingTarget || state.editingTarget.name !== name) return;
    var fresh = targets && targets.find(function (t) { return t.name === name; });
    if (!fresh) {
      toast(targets ? "该目标已不在列表中，无法重新加载" : "重新加载失败，请稍后重试", "error");
      return;
    }
    // Reopening resets every editor field and flag from the fresh target.
    openTargetModal(fresh);
  }
  function reloadButton() {
    return " <button type='button' class='btn btn-sm readonly-hide' data-reload-target='1'" +
      (ts.editingKeyBusy || ts.editingKeySaving ? " disabled" : "") + ">" +
      (ts.editingKeyReloading ? "正在载入…" : "重新加载") + "</button>";
  }
  function renderKeyList() {
    var container = document.getElementById("keyCardList");
    var notice = "";
    var isEditing = !!state.editingTarget;
    if (ts.editingKeySaving && !isEditing) {
      notice = "<p role='status'>目标创建中，暂不能修改密钥。</p>";
    }
    if (isEditing && ts.editingConflict) {
      notice = "<p role='alert'>该目标已被修改，请重新加载后再保存。草稿仍保留；重新加载会放弃当前草稿（包括尚未保存的新增密钥），用服务器最新内容替换整个表单。" +
        reloadButton() + "</p>";
    } else if (isEditing && ts.editingRemote === "changed") {
      notice = "<p role='status'>该目标已在别处修改。仍可保存，服务端会按版本裁决；如需基于最新内容修改，请重新加载（会放弃当前草稿）。" +
        reloadButton() + "</p>";
    } else if (isEditing && ts.editingRemote === "missing") {
      notice = "<p role='status'>该目标已不在目标列表中（可能已被删除或改名），保存可能失败。</p>";
    }
    if (isEditing && !ts.editingKeyIdsOk) {
      notice += "<p role='status'>密钥列表暂不可编辑，其他字段可正常保存。" + reloadButton() + "</p>";
    }
    if (isEditing && ts.editingKeyStatusStale) {
      notice += "<p role='status'>密钥状态刷新失败，卡片显示上次状态；请刷新后查看。</p>";
    }
    if (!ts.targetPoolsSnapshot || ts.targetPoolOptionsSnapshot !== ts.targetPoolsSnapshot) {
      notice += "<p role='status'>部署池数据不可用或选项可能过期，当前选择已保留；请重新打开编辑查看最新选项。</p>";
    }
    if (!ts.tgtKeyList.length && !notice) {
      container.style.display = "none";
      container.innerHTML = "";
      return;
    }
    container.style.display = "block";
    container.innerHTML = notice + ts.tgtKeyList
      .map(function (item, i) {
        var mask = item.status && item.status.key_mask ? item.status.key_mask : item.existing
          ? (item.value || "空主密钥槽位") : item.value.length <= 8 ? "****" : maskKey(item.value);
        var statusBadge = "";
        if (item.status) {
          statusBadge =
            "<span style='" +
            keyStatusColor(item.status.status) +
            ";padding:2px 8px;border-radius:10px;font-size:11px;font-weight:600;white-space:nowrap;'>" +
            esc(item.status.status) +
            "</span>";
        }
        var actions = "";
        if (isEditing && item.keyId && item.status && typeof item.status.blocked === "boolean" &&
            typeof item.status.exhausted === "boolean" && !ts.editingKeyStatusStale &&
            !ts.editingKeyBusy && !ts.editingKeySaving) {
          if (item.status.blocked || item.status.exhausted) {
            actions =
              "<button class='btn btn-sm readonly-hide' data-unblock-key-id='" +
              esc(item.keyId) +
              "' title='解除屏蔽' aria-label='解除屏蔽' style='padding:2px 8px;font-size:11px;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='4' y='11' width='16' height='10' rx='2'></rect><path d='M8 11V7a4 4 0 0 1 7.5-2'></path></svg></button>";
          } else {
            actions =
              "<button class='btn btn-sm readonly-hide' data-block-key-id='" +
              esc(item.keyId) +
              "' title='屏蔽' aria-label='屏蔽' style='padding:2px 8px;font-size:11px;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='4' y='11' width='16' height='10' rx='2'></rect><path d='M8 11V7a4 4 0 0 1 8 0v4'></path></svg></button>";
          }
        }
        var removeBtn =
          "<button class='btn btn-sm readonly-hide' data-remove-key-idx='" +
          i +
          "'" + ((ts.editingKeyReloading && item.existing) || ts.editingKeySaving || keysReadOnly() ? " disabled" : "") +
          " title='移除' aria-label='移除' style='padding:2px 8px;font-size:11px;color:var(--danger);'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><path d='M6 6l12 12M18 6L6 18'></path></svg></button>";
        var cooldownInfo = "";
        var itemCooldownEnd = keyCooldownEnd(item.status);
        if (item.status && item.status.exhausted && itemCooldownEnd) {
          var cd = new Date(itemCooldownEnd);
          var now = new Date();
          if (cd > now) {
            var mins = Math.ceil((cd - now) / 60000);
            cooldownInfo =
              "<span style='font-size:11px;color:var(--text-muted);'>" +
              mins +
              "min</span>";
          }
        }
        return (
          "<div style='display:flex;align-items:center;gap:8px;padding:6px 10px;border:1px solid var(--line);border-radius:6px;margin-bottom:4px;background:var(--surface-2);'>" +
          "<span style='font-family:var(--font-mono);font-size:12px;color:var(--text-secondary);flex:1;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;' title='" +
          esc(mask) +
          "'>" +
          esc(mask) +
          "</span>" +
          statusBadge +
          cooldownInfo +
          actions +
          removeBtn +
          "</div>"
        );
      })
      .join("");
  }

  // Editing a target whose key IDs are unusable: keys are shown but cannot change.
  function keysReadOnly() {
    return !!state.editingTarget && !ts.editingKeyIdsOk;
  }
  function addKeyToList() {
    var input = document.getElementById("tgtNewKeyInput");
    if (keysReadOnly()) {
      toast("密钥列表暂不可编辑，请先重新加载目标。", "error");
      return;
    }
    if (ts.editingKeySaving) {
      toast("保存进行中，请等待后再添加密钥。", "error");
      return;
    }
    var val = input.value.trim();
    if (!val) return;
    // dedup
    for (var i = 0; i < ts.tgtKeyList.length; i++) {
      if (ts.tgtKeyList[i].value === val) {
        toast("Key 已存在", "error");
        return;
      }
    }
    ts.tgtKeyList.push({ value: val, status: null });
    input.value = "";
    renderKeyList();
  }

  function removeKeyFromList(idx) {
    if (ts.editingKeySaving || keysReadOnly()) return;
    if (!Number.isInteger(idx) || idx < 0 || idx >= ts.tgtKeyList.length ||
        (ts.editingKeyReloading && ts.tgtKeyList[idx].existing)) return;
    ts.tgtKeyList.splice(idx, 1);
    renderKeyList();
  }

  document.getElementById("addKeyBtn").addEventListener("click", addKeyToList);
  document
    .getElementById("tgtNewKeyInput")
    .addEventListener("keydown", function (e) {
      if (e.key === "Enter") {
        e.preventDefault();
        addKeyToList();
      }
    });

  // Event delegation for key card actions (block/unblock/remove/reload)
  document
    .getElementById("keyCardList")
    .addEventListener("click", function (e) {
      var t = e.target.closest ? e.target.closest("button") || e.target : e.target;
      if (t.dataset.removeKeyIdx !== undefined) {
        removeKeyFromList(parseInt(t.dataset.removeKeyIdx, 10));
      }
      if (t.dataset.reloadTarget !== undefined && state.editingTarget && !ts.editingKeyBusy && !ts.editingKeySaving) {
        var reloadSession = ts.editingKeySession;
        var reloadName = state.editingTarget.name;
        showConfirm(
          "重新加载会放弃当前草稿（包括尚未保存的新增密钥和对已保存密钥的移除选择），用服务器最新内容替换整个表单。继续吗？",
          async function () {
            if (reloadSession === ts.editingKeySession && !ts.editingKeyBusy && !ts.editingKeySaving) {
              ts.editingKeyBusy = true;
              ts.editingKeyReloading = true;
              renderKeyList();
              try {
                await reloadEditingTarget(reloadSession, reloadName);
              } finally {
                if (reloadSession === ts.editingKeySession) {
                  ts.editingKeyBusy = false;
                  ts.editingKeyReloading = false;
                  renderKeyList();
                }
              }
            }
          },
        );
      }
      var block = t.dataset.blockKeyId !== undefined;
      if ((block || t.dataset.unblockKeyId !== undefined) && state.editingTarget &&
          !ts.editingKeyStatusStale && !ts.editingKeyBusy && !ts.editingKeySaving) {
        var keyId = block ? t.dataset.blockKeyId : t.dataset.unblockKeyId;
        var name = state.editingTarget.name;
        var session = ts.editingKeySession;
        showConfirm(block ? "确认屏蔽此 Key？屏蔽后该 Key 将不参与转发，直到手动解除。" : "确认解除屏蔽此 Key？解除后该 Key 将重新参与转发。", async function () {
          if (session !== ts.editingKeySession || ts.editingKeyBusy || ts.editingKeySaving ||
              !ts.tgtKeyList.some(function (item) { return item.keyId === keyId; })) return;
          ts.editingKeyBusy = true;
          renderKeyList();
          try {
            // Block state is not part of the stored target, so the edit ETag stays valid.
            await api("/admin/data/targets/" + encodeURIComponent(name) + "/keys/id/" + encodeURIComponent(keyId) +
              (block ? "/block" : "/unblock"), { method: "PUT" });
            if (session !== ts.editingKeySession) return;
            ts.editingKeyStatusStale = true;
            toast(block ? "已屏蔽 Key" : "已解除屏蔽", "success");
            await refreshEditingTarget();
          } catch (err) {
            if (session !== ts.editingKeySession) return;
            toast((block ? "屏蔽失败: " : "解除屏蔽失败: ") + err.message, "error");
          } finally {
            if (session === ts.editingKeySession) {
              ts.editingKeyBusy = false;
              renderKeyList();
            }
          }
        });
      }
    });
  return {
    refreshEditingTarget: refreshEditingTarget,
    renderKeyList: renderKeyList,
    addKeyToList: addKeyToList,
    removeKeyFromList: removeKeyFromList,
    updateEditingKeyStatuses: updateEditingKeyStatuses,
    syncEditingWithRefresh: syncEditingWithRefresh,
  };
});
