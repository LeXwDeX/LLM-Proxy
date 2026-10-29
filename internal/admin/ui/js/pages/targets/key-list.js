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
  var keyListFromSnapshot = app.bind("targetsHealth", "keyListFromSnapshot");
  var keyStatusColor = app.bind("targetsHealth", "keyStatusColor");
  var loadTargets = app.bind("targets", "loadTargets");
  var sameKeyIndices = app.bind("targetsHealth", "sameKeyIndices");
  var usableKeySnapshot = app.bind("targetsHealth", "usableKeySnapshot");
  ["input", "change"].forEach(function (eventName) {
    document.getElementById("targetModal").addEventListener(eventName, function () { ts.editingDraftRevision++; });
  });
  document.getElementById("targetModal").addEventListener("click", function (event) {
    var button = event.target.closest("button");
    if (ts.editingKeySaving && button && button.id !== "saveTargetBtn") ts.editingDraftRevision++;
  });
  function updateEditingKeyStatuses(t) {
    var statuses = Array.isArray(t.key_statuses) ? t.key_statuses : [];
    ts.tgtKeyList.forEach(function (item) {
      if (item.ref) item.status = Number.isInteger(item.runtimeIndex)
        ? statuses.find(function (s) { return s && s.index === item.runtimeIndex; }) || null : null;
    });
    ts.editingKeyStatusStale = false;
  }
  async function refreshEditingTarget() {
    var session = ts.editingKeySession;
    var name = state.editingTarget && state.editingTarget.name;
    var targets = await loadTargets();
    if (session !== ts.editingKeySession || !state.editingTarget || state.editingTarget.name !== name) return;
    if (!targets) {
      ts.editingKeyStatusStale = true;
    } else {
      var fresh = targets.find(function (t) { return t.name === name; });
      if (!fresh || fresh.key_snapshot !== ts.editingKeySnapshot || !sameKeyIndices(fresh)) {
        ts.editingKeyNeedsReview = true;
      } else if (!ts.editingKeyNeedsReview) {
        updateEditingKeyStatuses(fresh);
      }
    }
    renderKeyList();
  }

  async function reloadEditingKeys(session, name) {
    var targets = await loadTargets();
    if (session !== ts.editingKeySession || !state.editingTarget || state.editingTarget.name !== name) return;
    var fresh = targets && targets.find(function (t) { return t.name === name; });
    if (!usableKeySnapshot(fresh)) {
      ts.editingKeyNeedsReview = true;
      renderKeyList();
      toast("密钥快照不可用，请刷新后重新确认", "error");
      return;
    }
    var additions = ts.tgtKeyList.filter(function (item) { return !item.ref; });
    ts.tgtKeyList = keyListFromSnapshot(fresh).concat(additions);
    state.editingTarget = fresh;
    ts.editingKeySnapshot = fresh.key_snapshot;
    ts.editingKeyIndices = fresh.key_runtime_indices.slice();
    ts.editingKeyNeedsReview = false;
    ts.editingKeyStatusStale = false;
    renderKeyList();
  }
  function renderKeyList() {
    var container = document.getElementById("keyCardList");
    var notice = "";
    if (ts.editingKeySaving && !state.editingTarget) {
      notice = "<p role='status'>目标创建中，暂不能修改密钥。</p>";
    }
    if (ts.createdDraftNeedsReopen) {
      notice = "<p role='alert'>目标已创建，后续修改未保存。请复制草稿并重新打开目标编辑。</p>";
    }
    if (state.editingTarget && ts.editingKeyNeedsReview) {
      notice = "<p role='alert'>密钥快照已变化或不可用，草稿仍保留。请重新载入并确认；已移除的已保存密钥需要重新选择，新增密钥及其他表单草稿会保留。保存前请核对其他草稿，它们可能覆盖其他人更新的配置。 " +
        "<button type='button' class='btn btn-sm readonly-hide' data-reload-key-draft='1'" + (ts.editingKeyBusy ? " disabled" : "") + ">" +
        (ts.editingKeyReloading ? "正在载入…" : "重新载入密钥并确认") + "</button></p>";
    } else if (state.editingTarget && ts.editingKeyStatusStale) {
      notice = "<p role='status'>密钥状态刷新失败，卡片显示上次状态；请刷新后查看。</p>";
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
    var isEditing = !!state.editingTarget;
    container.innerHTML = notice + ts.tgtKeyList
      .map(function (item, i) {
        var mask = item.status && item.status.key_mask ? item.status.key_mask : item.ref
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
        if (isEditing && item.status && typeof item.status.blocked === "boolean" &&
            typeof item.status.exhausted === "boolean" && !ts.editingKeyNeedsReview && !ts.editingKeyStatusStale && !ts.editingKeyBusy) {
          var idx = item.status.index;
          if (item.status.blocked || item.status.exhausted) {
            actions =
              "<button class='btn btn-sm readonly-hide' data-unblock-key-idx='" +
              idx +
              "' title='解除屏蔽' aria-label='解除屏蔽' style='padding:2px 8px;font-size:11px;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='4' y='11' width='16' height='10' rx='2'></rect><path d='M8 11V7a4 4 0 0 1 7.5-2'></path></svg></button>";
          } else {
            actions =
              "<button class='btn btn-sm readonly-hide' data-block-key-idx='" +
              idx +
              "' title='屏蔽' aria-label='屏蔽' style='padding:2px 8px;font-size:11px;'><svg width='12' height='12' viewBox='0 0 24 24' fill='none' stroke='currentColor' stroke-width='2' stroke-linecap='round' stroke-linejoin='round' aria-hidden='true'><rect x='4' y='11' width='16' height='10' rx='2'></rect><path d='M8 11V7a4 4 0 0 1 8 0v4'></path></svg></button>";
          }
        }
        var removeBtn =
          "<button class='btn btn-sm readonly-hide' data-remove-key-idx='" +
          i +
          "'" + ((ts.editingKeyReloading && item.ref) || (ts.editingKeySaving && !isEditing) || ts.createdDraftNeedsReopen ? " disabled" : "") +
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

  function addKeyToList() {
    var input = document.getElementById("tgtNewKeyInput");
    if ((ts.editingKeySaving && !state.editingTarget) || ts.createdDraftNeedsReopen) {
      toast(ts.createdDraftNeedsReopen ? "目标已创建，请重新打开目标后再添加密钥。" : "创建请求进行中，请等待后再添加密钥。", "error");
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
    ts.editingDraftRevision++;
    input.value = "";
    renderKeyList();
  }

  function removeKeyFromList(idx) {
    if ((ts.editingKeySaving && !state.editingTarget) || ts.createdDraftNeedsReopen) return;
    if (!Number.isInteger(idx) || idx < 0 || idx >= ts.tgtKeyList.length ||
        (ts.editingKeyReloading && ts.tgtKeyList[idx].ref)) return;
    ts.tgtKeyList.splice(idx, 1);
    ts.editingDraftRevision++;
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

  // Event delegation for key card actions (block/unblock/remove)
  document
    .getElementById("keyCardList")
    .addEventListener("click", function (e) {
      var t = e.target.closest ? e.target.closest("button") || e.target : e.target;
      if (t.dataset.removeKeyIdx !== undefined) {
        removeKeyFromList(parseInt(t.dataset.removeKeyIdx, 10));
      }
      if (t.dataset.reloadKeyDraft !== undefined && state.editingTarget && !ts.editingKeyBusy) {
        var reloadSession = ts.editingKeySession;
        var reloadName = state.editingTarget.name;
        showConfirm(
          "重新载入已保存的密钥会重置本次密钥移除选择，请重新选择需要移除的密钥。新增密钥和其他表单草稿会保留，保存前请核对，以免覆盖其他人更新的配置。继续吗？",
          async function () {
            if (reloadSession === ts.editingKeySession && !ts.editingKeyBusy) {
              ts.editingKeyBusy = true;
              ts.editingKeyReloading = true;
              renderKeyList();
              try {
                await reloadEditingKeys(reloadSession, reloadName);
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
      var block = t.dataset.blockKeyIdx !== undefined;
      if ((block || t.dataset.unblockKeyIdx !== undefined) && state.editingTarget &&
          !ts.editingKeyNeedsReview && !ts.editingKeyStatusStale && !ts.editingKeyBusy && ts.editingKeySnapshot) {
        var idx = parseInt(block ? t.dataset.blockKeyIdx : t.dataset.unblockKeyIdx, 10);
        var name = state.editingTarget.name;
        var session = ts.editingKeySession;
        var snapshot = ts.editingKeySnapshot;
        showConfirm(block ? "确认屏蔽此 Key？屏蔽后该 Key 将不参与转发，直到手动解除。" : "确认解除屏蔽此 Key？解除后该 Key 将重新参与转发。", async function () {
          if (session !== ts.editingKeySession || ts.editingKeyBusy || ts.editingKeyNeedsReview || ts.editingKeyStatusStale ||
              snapshot !== ts.editingKeySnapshot || !ts.tgtKeyList.some(function (item) { return item.status && item.runtimeIndex === idx; })) return;
          var targetsBeforeOperation = state.targets;
          ts.editingKeyBusy = true;
          renderKeyList();
          try {
            var result = await api("/admin/data/targets/" + encodeURIComponent(name) + "/keys/" + idx + (block ? "/block" : "/unblock"),
              { method: "PUT", headers: { "X-Target-Key-Snapshot": snapshot } });
            if (session !== ts.editingKeySession) return;
            if (result && typeof result.key_snapshot === "string" && result.key_snapshot && result.key_snapshot !== snapshot) {
              ts.editingKeySnapshot = result.key_snapshot;
            } else {
              ts.editingKeyNeedsReview = true;
            }
            ts.editingKeyStatusStale = true;
            toast(block ? "已屏蔽 Key" : "已解除屏蔽", "success");
            await refreshEditingTarget();
          } catch (err) {
            if (session !== ts.editingKeySession) return;
            ts.editingKeyNeedsReview = true;
            toast("操作失败，密钥草稿已保留，请重新确认", "error");
          } finally {
            if (session === ts.editingKeySession) {
              if (state.targets !== targetsBeforeOperation) {
                var latest = state.targets.find(function (item) { return item.name === name; });
                if (!latest || latest.key_snapshot !== ts.editingKeySnapshot || !sameKeyIndices(latest)) {
                  ts.editingKeyNeedsReview = true;
                }
              }
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
  };
});
