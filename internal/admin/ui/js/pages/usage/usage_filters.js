"use strict";
// Usage filters: client and model-group selectors and the model-group list loader.
AdminApp.register("usageFilters", function (app) {
  var state = app.state;
  var modelGroupLabel = app.bind("quota", "modelGroupLabel");
  var api = app.bind("core", "api");
  var loadUsage = app.bind("usage", "loadUsage");
  function populateUsageClientFilter(activeClients) {
    var sel = document.getElementById("usageClientFilter");
    var current = sel.value;
    sel.innerHTML = '<option value="">全部客户端</option>';
    var list = activeClients || [];
    list
      .slice()
      .sort(function (a, b) {
        return String(a).localeCompare(String(b));
      })
      .forEach(function (name) {
        var opt = document.createElement("option");
        opt.value = name;
        opt.textContent = name;
        sel.appendChild(opt);
      });
    if (current && list.indexOf(current) >= 0) {
      sel.value = current;
    } else {
      sel.value = "";
    }
  }
  function populateUsageModelGroupFilter(groups) {
    var menu = document.getElementById("usageModelGroupFilterMenu");
    var selected = usageModelGroupValues();
    var names = (groups || [])
      .map(function (g) {
        return typeof g === "string" ? g : g.name;
      })
      .filter(Boolean);
    names = names
      .filter(function (name, i, all) {
        return all.indexOf(name) === i;
      })
      .sort(function (a, b) {
        return String(a).localeCompare(String(b));
      });
    selected = selected.filter(function (name) {
      return names.indexOf(name) >= 0;
    });
    menu.innerHTML = "";
    appendUsageModelGroupOption(menu, "", "全部模型组", selected.length === 0);
    names.forEach(function (name) {
      appendUsageModelGroupOption(
        menu,
        name,
        modelGroupLabel(name),
        selected.indexOf(name) >= 0,
      );
    });
    updateUsageModelGroupFilterButton();
  }
  function appendUsageModelGroupOption(menu, value, label, checked) {
    var wrapper = document.createElement("label");
    wrapper.className = "usage-model-group-option";
    var input = document.createElement("input");
    input.type = "checkbox";
    input.value = value;
    input.checked = checked;
    input.setAttribute("data-model-group-value", value);
    wrapper.appendChild(input);
    var text = document.createElement("span");
    text.textContent = label;
    wrapper.appendChild(text);
    menu.appendChild(wrapper);
  }
  function updateUsageModelGroupFilterButton() {
    var button = document.getElementById("usageModelGroupFilterButton");
    var groups = usageModelGroupValues();
    button.textContent =
      groups.length === 0
        ? "全部模型组"
        : groups.length === 1
          ? modelGroupLabel(groups[0])
          : "已选 " + groups.length + " 个模型组";
  }
  function refreshUsageModelGroupFilter() {
    populateUsageModelGroupFilter(state.modelGroups || []);
  }
  function ensureModelGroupsLoaded() {
    if (state.modelGroupsLoaded) return Promise.resolve(state.modelGroups);
    if (state.modelGroupsPromise) return state.modelGroupsPromise;
    state.modelGroupsPromise = api("/admin/data/model-groups")
      .then(function (data) {
        if (state.modelGroupsLoaded) return state.modelGroups;
        state.modelGroups = Array.isArray(data.model_groups)
          ? data.model_groups
          : [];
        state.modelGroupsLoaded = true;
        refreshUsageModelGroupFilter();
        return state.modelGroups;
      })
      .catch(function (error) {
        if (state.modelGroupsLoaded) return state.modelGroups;
        throw error;
      })
      .finally(function () {
        state.modelGroupsPromise = null;
      });
    return state.modelGroupsPromise;
  }
  function usageModelGroupValues() {
    return Array.prototype.slice
      .call(
        document.querySelectorAll(
          "#usageModelGroupFilterMenu input[type=checkbox][data-model-group-value]",
        ),
      )
      .filter(function (input) {
        return input.checked && input.value;
      })
      .map(function (input) {
        return input.value;
      });
  }
  function appendUsageModelGroups(qs, groups) {
    groups.forEach(function (name) {
      qs.append("model_group", name);
    });
  }
  document
    .getElementById("usageClientFilter")
    .addEventListener("change", function () {
      loadUsage();
    });
  document
    .getElementById("usageModelGroupFilterButton")
    .addEventListener("click", function () {
      var menu = document.getElementById("usageModelGroupFilterMenu");
      var open = menu.hidden;
      menu.hidden = !open;
      this.setAttribute("aria-expanded", String(open));
      if (open) {
        var first = menu.querySelector("input");
        if (first) first.focus();
      }
    });
  document
    .getElementById("usageModelGroupFilterMenu")
    .addEventListener("change", function (event) {
      var input = event.target;
      if (!input || input.type !== "checkbox") return;
      var inputs = Array.prototype.slice.call(
        this.querySelectorAll("input[type=checkbox]"),
      );
      if (!input.value && input.checked) {
        inputs.forEach(function (option) {
          option.checked = option === input;
        });
      } else if (input.value && input.checked) {
        inputs[0].checked = false;
      }
      if (!usageModelGroupValues().length) {
        inputs[0].checked = true;
      }
      updateUsageModelGroupFilterButton();
      loadUsage();
    });
  document.addEventListener("click", function (event) {
    var filter = document.getElementById("usageModelGroupFilter");
    if (filter && !filter.contains(event.target)) {
      var menu = document.getElementById("usageModelGroupFilterMenu");
      var button = document.getElementById("usageModelGroupFilterButton");
      menu.hidden = true;
      button.setAttribute("aria-expanded", "false");
    }
  });
  document
    .getElementById("usageModelGroupFilter")
    .addEventListener("keydown", function (event) {
      if (event.key === "Escape") {
        var menu = document.getElementById("usageModelGroupFilterMenu");
        var button = document.getElementById("usageModelGroupFilterButton");
        menu.hidden = true;
        button.setAttribute("aria-expanded", "false");
        button.focus();
      }
    });

  window.refreshUsageModelGroupFilter = refreshUsageModelGroupFilter;
  return {appendUsageModelGroupOption: appendUsageModelGroupOption, appendUsageModelGroups: appendUsageModelGroups, ensureModelGroupsLoaded: ensureModelGroupsLoaded, populateUsageClientFilter: populateUsageClientFilter, populateUsageModelGroupFilter: populateUsageModelGroupFilter, refreshUsageModelGroupFilter: refreshUsageModelGroupFilter, updateUsageModelGroupFilterButton: updateUsageModelGroupFilterButton, usageModelGroupValues: usageModelGroupValues};
});
