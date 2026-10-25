"use strict";
// Schema owns metadata validation, select options and the read-only compatibility summary.
// Imports resolve through AdminApp; implementation state stays inside this module.
AdminApp.register("schema", function (app) {
  var state = app.state;
  var api = app.bind("core", "api");
  var esc = app.bind("core", "esc");
  /* ===== Targets ===== */
  // endpoint_type 元数据由 GET /admin/data/schema 在 bootstrap 时注入到
  // state.endpointTypeMetas，下面的 helper 全部从该缓存派生，禁止再硬编码列表。
  function epMeta(code) {
    var c = code || state.schema.defaults.endpoint_type;
    var metas = state.endpointTypeMetas || [];
    for (var i = 0; i < metas.length; i++) {
      if (metas[i].code === c) return metas[i];
    }
    return null;
  }
  function epDisplayName(code) {
    var m = epMeta(code);
    return m ? m.display_name : code || "";
  }
  function epShortLabel(code) {
    var m = epMeta(code);
    return m ? m.short_label : code || "";
  }
  function epBadgeStyle(code) {
    var m = epMeta(code);
    if (m)
      return (
        "background:" + m.badge_background + ";color:" + m.badge_foreground
      );
    return "background:var(--muted-soft);color:var(--ink-3)";
  }

  // A failed or partial schema must never turn form fields into empty options.
  function validateSchema(data) {
    if (!data || data.version !== 1)
      throw new Error("不支持的管理 Schema 版本");
    [
      "endpoint_types",
      "protocols",
      "request_schemas",
      "usage_adapters",
      "image_operations",
      "auth_modes",
      "pricing_modes",
      "pricing_service_tiers",
      "pricing_speeds",
      "roles",
      "quota_scopes",
      "quota_sync_modes",
      "quota_dimensions",
      "target_phases",
    ].forEach(function (key) {
      if (!Array.isArray(data[key]) || !data[key].length)
        throw new Error("Schema 缺少 " + key);
      var seen = new Set();
      data[key].forEach(function (option) {
        var value = key === "endpoint_types" ? option.code : option.value;
        var label =
          key === "endpoint_types" ? option.display_name : option.label;
        if (
          typeof value !== "string" ||
          typeof label !== "string" ||
          !label ||
          seen.has(value)
        )
          throw new Error("Schema 选项无效: " + key);
        seen.add(value);
        if (
          key === "quota_dimensions" &&
          (!option.base_field || !option.job_coefficient_field)
        )
          throw new Error("Schema 额度维度字段不完整");
        if (key === "endpoint_types") {
          if (
            !Array.isArray(option.protocols) ||
            !Array.isArray(option.request_schemas)
          )
            throw new Error("Schema 协议能力不完整");
          [
            "supports_auth_mode",
            "supports_image_operation",
            "supports_bearer_passthrough",
            "supports_dual_protocol",
          ].forEach(function (flag) {
            if (typeof option[flag] !== "boolean")
              throw new Error("Schema 上游能力缺少 " + flag);
          });
        }
      });
    });
    if (!data.defaults) throw new Error("Schema 缺少 defaults");
    var defaults = {
      endpoint_type: "endpoint_types",
      usage_adapter: "usage_adapters",
      image_operation: "image_operations",
      auth_mode: "auth_modes",
      pricing_mode: "pricing_modes",
      role: "roles",
    };
    Object.keys(defaults).forEach(function (key) {
      var list = data[defaults[key]],
        field = key === "endpoint_type" ? "code" : "value";
      if (
        typeof data.defaults[key] !== "string" ||
        (!(key === "usage_adapter" && data.defaults[key] === "") &&
          !list.some(function (item) {
            return item[field] === data.defaults[key];
          }))
      )
        throw new Error("Schema 默认值无效: " + key);
    });
    if (!data.defaults.pricing_timezone) throw new Error("Schema 缺少计价时区");
    ["openai_prefix", "anthropic_prefix"].forEach(function (key) {
      if (typeof data.defaults[key] !== "string")
        throw new Error("Schema 默认值无效: " + key);
    });
    if (typeof data.defaults.supports_responses !== "boolean")
      throw new Error("Schema 默认值无效: supports_responses");
    if (typeof data.defaults.request_stream_usage_if_missing !== "boolean")
      throw new Error("Schema 默认值无效: request_stream_usage_if_missing");
    if (typeof data.stream_usage_request_schema !== "string" || !data.request_schemas.some(function (item) {
      return item.value === data.stream_usage_request_schema;
    })) throw new Error("Schema 流式用量适用条件无效");
    return data;
  }
  function schemaLabel(list, value) {
    var entry = (state.schema[list] || []).find(function (option) {
      return option.value === value;
    });
    return entry ? entry.label : value || "未知";
  }
  function setSchemaSelectValue(id, value) {
    var select = document.getElementById(id);
    var raw = value == null ? "" : String(value);
    // Preserve future/legacy configured values rather than silently saving an empty field.
    if (
      !Array.from(select.options).some(function (option) {
        return option.value === raw;
      })
    ) {
      var option = document.createElement("option");
      option.value = raw;
      option.textContent = "未识别配置: " + raw;
      select.appendChild(option);
    }
    select.value = raw;
  }
  function populateSchemaOptions() {
    document
      .querySelectorAll("select.endpoint-type-select")
      .forEach(function (select) {
        var includeEmpty = select.getAttribute("data-include-empty");
        var html =
          includeEmpty !== null
            ? '<option value="">' + esc(includeEmpty) + "</option>"
            : "";
        state.schema.endpoint_types.forEach(function (meta) {
          html +=
            '<option value="' +
            esc(meta.code) +
            '">' +
            esc(meta.display_name) +
            "</option>";
        });
        select.innerHTML = html;
        if (includeEmpty === null)
          select.value = state.schema.defaults.endpoint_type;
      });
    var defaults = {
      usage_adapters: "usage_adapter",
      auth_modes: "auth_mode",
      image_operations: "image_operation",
      roles: "role",
      pricing_modes: "pricing_mode",
    };
    document
      .querySelectorAll("select[data-schema-options]")
      .forEach(function (select) {
        var key = select.getAttribute("data-schema-options");
        if (!Array.isArray(state.schema[key]))
          throw new Error("未知 Schema 选项集: " + key);
        select.innerHTML =
          (key === "usage_adapters"
            ? '<option value="">请选择计费类型</option>'
            : "") +
          state.schema[key]
            .map(function (option) {
              return (
                '<option value="' +
                esc(option.value) +
                '">' +
                esc(option.label) +
                "</option>"
              );
            })
            .join("");
        if (defaults[key]) select.value = state.schema.defaults[defaults[key]];
      });
    document.getElementById("ptTimezone").value =
      state.schema.defaults.pricing_timezone;
    document.getElementById("ptTimezone").placeholder =
      state.schema.defaults.pricing_timezone;
    renderSchemaSummary();
    document
      .querySelectorAll("[data-schema-dimension]")
      .forEach(function (element) {
        var dim = state.schema.quota_dimensions.find(function (item) {
          return item.value === element.getAttribute("data-schema-dimension");
        });
        if (dim)
          element.textContent =
            dim.label + (element.getAttribute("data-schema-suffix") || "");
      });
  }

  function renderSchemaSummary() {
    var lists = {
      protocols: "协议",
      request_schemas: "请求格式",
      usage_adapters: "计费类型",
      image_operations: "图像操作",
      auth_modes: "认证方式",
      pricing_modes: "计价模式",
      roles: "管理角色",
      quota_scopes: "额度范围",
      quota_sync_modes: "额度同步",
      target_phases: "目标运行状态",
    };
    var rows = [];
    state.schema.endpoint_types.forEach(function (meta) {
      var detail =
        "协议：" +
        meta.protocols
          .map(function (value) {
            return schemaLabel("protocols", value);
          })
          .join(" / ") +
        "；请求格式：" +
        meta.request_schemas
          .map(function (value) {
            return schemaLabel("request_schemas", value);
          })
          .join(" / ");
      rows.push(
        "<tr><td>上游类型</td><td><code>" +
          esc(meta.code) +
          "</code></td><td>" +
          esc(meta.display_name + " · " + detail) +
          "</td></tr>",
      );
    });
    Object.keys(lists).forEach(function (key) {
      state.schema[key].forEach(function (option) {
        rows.push(
          "<tr><td>" +
            esc(lists[key]) +
            "</td><td><code>" +
            esc(option.value || "（默认空值）") +
            "</code></td><td>" +
            esc(option.label) +
            "</td></tr>",
        );
      });
    });
    state.schema.quota_dimensions.forEach(function (dim) {
      rows.push(
        "<tr><td>额度维度</td><td><code>" +
          esc(dim.value) +
          "</code></td><td>" +
          esc(
            dim.label +
              " · 基数 " +
              dim.base_field +
              " · 职级倍率 " +
              dim.job_coefficient_field,
          ) +
          "</td></tr>",
      );
    });
    Object.keys(state.schema.defaults).forEach(function (key) {
      rows.push(
        "<tr><td>默认值</td><td><code>" +
          esc(key) +
          "</code></td><td>" +
          esc(state.schema.defaults[key] === "" ? "（空值）" : String(state.schema.defaults[key])) +
          "</td></tr>",
      );
    });
    document.getElementById("adminSchemaSummary").innerHTML =
      "<div class='table-wrap'><table><thead><tr><th>配置分类</th><th>配置值</th><th>显示名与兼容能力</th></tr></thead><tbody>" +
      rows.join("") +
      "</tbody></table></div>";
  }

  async function loadSchema() {
    var schema = validateSchema(await api("/admin/data/schema"));
    state.schema = schema;
    state.endpointTypeMetas = schema.endpoint_types;
    populateSchemaOptions();
    return schema;
  }

  window.epDisplayName = epDisplayName;
  return {
    renderSchemaSummary: renderSchemaSummary,
    epMeta: epMeta,
    epDisplayName: epDisplayName,
    epShortLabel: epShortLabel,
    epBadgeStyle: epBadgeStyle,
    loadSchema: loadSchema,
    validateSchema: validateSchema,
    schemaLabel: schemaLabel,
    setSchemaSelectValue: setSchemaSelectValue,
    populateSchemaOptions: populateSchemaOptions,
  };
});
AdminApp.initializeModule("schema");
