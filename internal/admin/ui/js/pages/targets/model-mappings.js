"use strict";
// Targets model mappings, catalog suggestions, custom headers and custom body editors.
// Part of the targets page; shared mutable state lives in the object owned by the "targets" module.
AdminApp.register("targetsMappings", function (app) {
  var ts = app.initializeModule("targets").shared;
  var state = app.state;
  var toast = app.bind("core", "toast");
  var api = app.bind("core", "api");
  var loadCatalog = app.bind("catalog", "loadCatalog");
  var catalogFilterModels = app.bind("catalog", "catalogFilterModels");
  // Each call supersedes earlier ones: a slow catalog response for an old type
  // or filter must not append its tags after a newer render.
  var suggestionSequence = 0;
  async function renderTgtModelSuggestions(epType, filterText) {
    var sequence = ++suggestionSequence;
    var container = document.getElementById("tgtModelSuggestions");
    container.innerHTML = "";
    var models = await loadCatalog(epType);
    if (sequence !== suggestionSequence) return;
    var filtered = catalogFilterModels(models, filterText);
    filtered.slice(0, 50).forEach(function (m) {
      var tag = document.createElement("span");
      tag.style.cssText =
        "display:inline-block;padding:2px 8px;border-radius:4px;background:var(--primary-light);color:var(--primary);font-size:12px;cursor:pointer;";
      tag.textContent = m.model || m.display_name || "";
      tag.title = m.display_name || m.model;
      tag.addEventListener("click", function () {
        // The editor is read-only while a save is in flight; a suggestion is not a button.
        if (ts.editingKeySaving) return;
        var modelName = m.model;
        // Skip if this model already exists in some row
        var existingInputs = document.querySelectorAll(
          "#model-mappings-container .upstream-input",
        );
        for (var i = 0; i < existingInputs.length; i++) {
          if (existingInputs[i].value.trim() === modelName) return;
        }
        addModelMappingRow(modelName, "", !!state.editingTarget);
      });
      container.appendChild(tag);
    });
    if (filterText && filterText.trim() && filtered.length === 0) {
      container.innerHTML =
        "<span style='color:var(--text-muted);font-size:12px;'>无匹配模型</span>";
    }
  }
  // addModelMappingRow creates a new row in the model-mappings editor.
  // Uses DOM API (createElement + .value) to prevent XSS from special characters.
  // showTest: 仅在编辑已保存的 target 时显示测试/暂停按钮（新建时无 target 名无法测试）。
  // paused: 该模型已暂停（不可被请求），行内显示删除线并可一键恢复。
  function addModelMappingRow(upstream, fallback, showTest, paused) {
    var container = document.getElementById("model-mappings-container");
    var row = document.createElement("div");
    row.className = "model-mapping-row";

    var upstreamInput = document.createElement("input");
    upstreamInput.type = "text";
    upstreamInput.className = "upstream-input";
    upstreamInput.placeholder = "上游名称（必填）";
    upstreamInput.value = upstream || "";
    upstreamInput.setAttribute("list", "catalogModelDatalist"); // catalog 软建议，保留自由输入（上游私有模型名不在目录内）
    upstreamInput.autocomplete = "off";

    var fallbackInput = document.createElement("input");
    fallbackInput.type = "text";
    fallbackInput.className = "fallback-input";
    fallbackInput.placeholder = "下游兜底（可选）";
    fallbackInput.value = fallback || "";

    var removeBtn = document.createElement("button");
    removeBtn.type = "button";
    removeBtn.className = "btn-remove-row";
    removeBtn.textContent = "\u00d7";
    removeBtn.addEventListener("click", function () {
      row.remove();
    });

    function applyPausedStyle() {
      var isPaused = row.dataset.paused === "1";
      upstreamInput.style.textDecoration = isPaused ? "line-through" : "";
      upstreamInput.style.opacity = isPaused ? "0.55" : "";
      fallbackInput.style.opacity = isPaused ? "0.55" : "";
    }
    row.dataset.paused = paused ? "1" : "";
    applyPausedStyle();

    row.appendChild(upstreamInput);
    row.appendChild(fallbackInput);
    if (showTest) {
      var pauseBtn = document.createElement("button");
      pauseBtn.type = "button";
      pauseBtn.className = "btn btn-sm pause-model-btn";
      pauseBtn.title =
        "暂停后该模型（含别名）不可被请求；与负载均衡无关，仅控制可用性";
      function renderPauseBtn() {
        var isPaused = row.dataset.paused === "1";
        pauseBtn.textContent = isPaused ? "恢复" : "暂停";
        pauseBtn.classList.toggle("btn-danger", !isPaused);
      }
      renderPauseBtn();
      pauseBtn.addEventListener("click", function () {
        row.dataset.paused = row.dataset.paused === "1" ? "" : "1";
        renderPauseBtn();
        applyPausedStyle();
      });
      row.appendChild(pauseBtn);
      var testBtn = document.createElement("button");
      testBtn.type = "button";
      testBtn.className = "btn btn-sm test-model-btn";
      testBtn.textContent = "测试";
      testBtn.title = "此检测发送最小 HTTP 请求检查上游响应；不验证 WebSocket 通道";
      testBtn.addEventListener("click", function () {
        testModelForRow(testBtn, upstreamInput.value.trim());
      });
      row.appendChild(testBtn);
    }
    row.appendChild(removeBtn);
    container.appendChild(row);
  }
  // testModelForRow 调用后端 test-model API 并弹 toast 反馈结果。
  function testModelForRow(btn, model) {
    if (!model) {
      toast("请先填写上游模型名", "error");
      return;
    }
    var targetName = state.editingTarget ? state.editingTarget.name : "";
    if (!targetName) {
      toast("请先保存目标后再测试模型", "error");
      return;
    }
    var originalText = btn.textContent;
    btn.disabled = true;
    btn.textContent = "测试中...";
    api(
      "/admin/data/targets/" + encodeURIComponent(targetName) + "/test-model",
      {
        method: "POST",
        body: { model: model },
        // Server bounds each upstream attempt (60s) and may rotate keys/paths.
        timeoutMs: 0,
      },
    )
      .then(function (res) {
        if (res && res.success) {
          toast(
            "HTTP 检测通过（HTTP " +
              res.status_code +
              ", " +
              res.latency_ms +
              "ms）",
            "success",
          );
        } else {
          var errMsg = res && res.error ? res.error : "未知错误";
          var status =
            res && res.status_code ? " (HTTP " + res.status_code + ")" : "";
          toast("测试失败: " + errMsg + status, "error");
        }
      })
      .catch(function (e) {
        toast("测试请求失败: " + e.message, "error");
      })
      .finally(function () {
        btn.disabled = false;
        btn.textContent = originalText;
      });
  }
  // collectModelMappings reads all non-empty rows from the editor.
  // Returns array suitable for the model_mappings API field.
  function collectModelMappings() {
    var mappings = [];
    var rows = document.querySelectorAll(
      "#model-mappings-container .model-mapping-row",
    );
    rows.forEach(function (row) {
      var upstream = row.querySelector(".upstream-input").value.trim();
      var fallback = row.querySelector(".fallback-input").value.trim();
      if (upstream) {
        var mapping = { upstream: upstream };
        if (fallback) {
          mapping.fallback = fallback;
        }
        if (row.dataset.paused === "1") {
          mapping.paused = true;
        }
        mappings.push(mapping);
      }
    });
    return mappings;
  }

  function updateModelPrefixPreview() {
    var prefix = document.getElementById("tgtModelPrefix").value;
    var first = document.getElementById("model-mappings-container").querySelector(".upstream-input");
    var model = first ? first.value.trim() : "";
    document.getElementById("tgtModelPrefixPreview").textContent = model
      ? "上游模型名预览：" + prefix + model
      : "填写一行模型后，可预览发给上游的完整模型名。";
  }
  document.getElementById("tgtModelPrefix").addEventListener("input", updateModelPrefixPreview);
  document.getElementById("model-mappings-container").addEventListener("input", updateModelPrefixPreview);
  // ===== Custom Headers KV editor =====
  // Rows are built with createElement and .value: user text is never parsed as
  // markup, so "&amp;" and quotes survive a load/save round trip unchanged.
  function addKvRow(container, namePlaceholder, valuePlaceholder, valueStyle, name, value) {
    var row = document.createElement("div");
    row.style.cssText =
      "display:flex;gap:8px;margin-bottom:4px;align-items:center;";
    var nameInput = document.createElement("input");
    nameInput.className = "form-control";
    nameInput.placeholder = namePlaceholder;
    nameInput.style.cssText = "flex:1;font-size:13px;";
    nameInput.value = name || "";
    var valueInput = document.createElement("input");
    valueInput.className = "form-control";
    valueInput.placeholder = valuePlaceholder;
    valueInput.style.cssText = "flex:2;font-size:13px;" + valueStyle;
    valueInput.value = value || "";
    var removeBtn = document.createElement("button");
    removeBtn.type = "button";
    removeBtn.className = "btn btn-sm btn-danger";
    removeBtn.style.cssText = "padding:2px 8px;";
    removeBtn.textContent = "\u00d7";
    removeBtn.addEventListener("click", function () {
      row.remove();
    });
    row.appendChild(nameInput);
    row.appendChild(valueInput);
    row.appendChild(removeBtn);
    container.appendChild(row);
  }
  function addCustomHeaderRow(name, value) {
    addKvRow(document.getElementById("custom-headers-container"),
      "Header Name", "Value", "", name, value);
  }
  document
    .getElementById("add-custom-header")
    .addEventListener("click", function () {
      addCustomHeaderRow("", "");
    });
  function collectCustomHeaders() {
    // Null-prototype map: a field named "__proto__" must remain a data member.
    var headers = Object.create(null);
    document
      .querySelectorAll("#custom-headers-container > div")
      .forEach(function (row) {
        var inputs = row.querySelectorAll("input");
        var name = inputs[0].value.trim();
        var value = inputs[1].value;
        if (name) headers[name] = value;
      });
    // 始终返回对象（含空 {}）：update 时让 handler 真正清空字段，create 时空 map 由 omitempty 省略
    return headers;
  }

  // ===== Custom Body KV editor =====
  function addCustomBodyRow(name, value) {
    addKvRow(document.getElementById("custom-body-container"),
      "Field Name", "Value (JSON)", "font-family:var(--font-mono);", name, value);
  }
  document
    .getElementById("add-custom-body")
    .addEventListener("click", function () {
      addCustomBodyRow("", "");
    });
  function collectCustomBody() {
    // Null-prototype map: a field named "__proto__" must remain a data member.
    var body = Object.create(null);
    document
      .querySelectorAll("#custom-body-container > div")
      .forEach(function (row) {
        var inputs = row.querySelectorAll("input");
        var name = inputs[0].value.trim();
        var rawValue = inputs[1].value;
        if (!name) return;
        // Try to parse as JSON; if fails, use as string
        try {
          body[name] = JSON.parse(rawValue);
        } catch (e) {
          body[name] = rawValue;
        }
      });
    // 始终返回对象（含空 {}）：update 时让 handler 真正清空字段，create 时空 map 由 omitempty 省略
    return body;
  }
  // collectCustomBodyRaw 只做 JSON 语法校验，保存原始输入文本。
  // 不能使用 JSON.parse 的结果：超过 Number.MAX_SAFE_INTEGER 的整数会丢失精度。
  // 无效 JSON 维持旧行为，按普通字符串提交。
  function collectCustomBodyRaw() {
    // Null-prototype map: a field named "__proto__" must remain a data member.
    var body = Object.create(null);
    document
      .querySelectorAll("#custom-body-container > div")
      .forEach(function (row) {
        var inputs = row.querySelectorAll("input");
        var name = inputs[0].value.trim();
        var rawValue = inputs[1].value;
        if (!name) return;
        try {
          JSON.parse(rawValue);
          body[name] = rawValue;
        } catch (e) {
          body[name] = JSON.stringify(rawValue);
        }
      });
    return body;
  }
  var tgtModelDebounce = null;
  // Delegated input listener: filter suggestions based on any upstream-input being typed
  document
    .getElementById("model-mappings-container")
    .addEventListener("input", function (e) {
      if (
        e.target &&
        e.target.classList &&
        e.target.classList.contains("upstream-input")
      ) {
        clearTimeout(tgtModelDebounce);
        var val = e.target.value;
        tgtModelDebounce = setTimeout(function () {
          var epType = document.getElementById("tgtType").value;
          renderTgtModelSuggestions(epType, val);
        }, 250);
      }
    });
  document
    .getElementById("add-model-mapping")
    .addEventListener("click", function () {
      addModelMappingRow("", "", !!state.editingTarget);
    });
  return {
    renderTgtModelSuggestions: renderTgtModelSuggestions,
    addModelMappingRow: addModelMappingRow,
    testModelForRow: testModelForRow,
    collectModelMappings: collectModelMappings,
    addCustomHeaderRow: addCustomHeaderRow,
    collectCustomHeaders: collectCustomHeaders,
    addCustomBodyRow: addCustomBodyRow,
    collectCustomBody: collectCustomBody,
    collectCustomBodyRaw: collectCustomBodyRaw,
    updateModelPrefixPreview: updateModelPrefixPreview,
  };
});
