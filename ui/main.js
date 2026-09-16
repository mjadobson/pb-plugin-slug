const SLUG_COLLECTION = "_plugins_slug";
const SLUG_EXTENSION = "pb-plugin-slug";

if (!document.querySelector("link[data-pb-plugin-slug-style]")) {
  document.head.appendChild(t.link({
    rel: "stylesheet",
    href: `/_/extensions/${SLUG_EXTENSION}/style.css`,
    "html-data-pb-plugin-slug-style": "true",
  }));
}

function fieldOptions(collections, collectionName) {
  const collection = collections.find((item) => item.name === collectionName);
  if (!collection) return [];

  const result = (collection.fields || [])
    .filter((field) => !field.system)
    .map((field) => ({ value: field.name, label: field.name, type: field.type }));

  for (const field of collection.fields || []) {
    if (field.type !== "relation" || !field.collectionId) continue;
    const related = collections.find((item) => item.id === field.collectionId);
    for (const relatedField of related?.fields || []) {
      if (!relatedField.system) {
        result.push({
          value: `${field.name}.${relatedField.name}`,
          label: `${field.name} → ${relatedField.name}`,
          type: relatedField.type,
        });
      }
    }
  }
  return result;
}

function blankRule() {
  return { id: "", collection_name: "", input_fields: [], output_field: "", length: 64, recalculating: false, recalculation_error: "", _heading: null };
}

function hydrateRule(rule) {
  return {
    ...rule,
    input_fields: Array.isArray(rule.input_fields) ? rule.input_fields : [],
    _heading: {
      collection: rule.collection_name || "Collection",
      inputs: Array.isArray(rule.input_fields) ? rule.input_fields : [],
      output: rule.output_field || "Output field",
    },
  };
}

function apiErrorMessage(err, fallback) {
  const fieldErrors = err?.response?.data;
  if (fieldErrors && typeof fieldErrors === "object") {
    const messages = Object.values(fieldErrors)
      .map((item) => item?.message)
      .filter(Boolean);
    if (messages.length) return messages.join(" ");
  }
  return err?.response?.message || err?.message || fallback;
}

const definition = {
  name: "slugify",
  author: "mjadobson",
  icon: "ri-links-line",
  label: "Slugify",

  async mount(_container, { app, t, store, signal }) {
    const state = store({ rules: [], collections: [], loading: true, saving: false });
    const records = app.pb.collection(SLUG_COLLECTION);

    const load = async () => {
      state.loading = true;
      try {
        const [rules, collections] = await Promise.all([
          records.getFullList({ sort: "collection_name,output_field", requestKey: null }),
          app.pb.collections.getFullList({ sort: "name", requestKey: null }),
        ]);
        if (signal.aborted) return;
        state.rules = rules.map(hydrateRule);
        state.collections = collections.filter((collection) => !collection.system && collection.type !== "view");
      } catch (err) {
        if (!err?.isAbort) app.toasts.error(apiErrorMessage(err, "Unable to load slug configuration."));
      } finally {
        state.loading = false;
      }
    };

    const save = async (rule) => {
      state.saving = true;
      const payload = {
        collection_name: rule.collection_name,
        input_fields: rule.input_fields,
        output_field: rule.output_field,
        length: Number(rule.length),
      };
      try {
        rule.id ? await records.update(rule.id, payload) : await records.create(payload);
        await load();
        app.toasts.success("Slug rule saved.");
      } catch (err) {
        app.toasts.error(apiErrorMessage(err, "Unable to save the slug rule."));
      } finally {
        state.saving = false;
      }
    };

    const remove = async (rule) => {
      if (!rule.id) {
        state.rules = state.rules.filter((item) => item !== rule);
        return;
      }
      app.modals.confirm("Delete this slug rule?", async () => {
        try {
          await records.delete(rule.id);
          await load();
        } catch (err) {
          app.toasts.error(apiErrorMessage(err, "Unable to delete the slug rule."));
        }
      });
    };

    const recalculate = async (rule) => {
      if (!rule.id || rule.recalculating) return;
      rule.recalculating = true;
      try {
        await records.update(rule.id, { recalculating: true, recalculation_error: "" });
      } catch (err) {
        rule.recalculating = false;
        app.toasts.error(apiErrorMessage(err, "Unable to start slug recalculation."));
      }
    };

    await load();
    let unsubscribeRealtime = null;
    if (!signal.aborted) {
      records.subscribe("*", (event) => {
        const index = state.rules.findIndex((rule) => rule.id === event.record.id);
        if (event.action === "delete") {
          if (index >= 0) state.rules.splice(index, 1);
        } else if (index >= 0) {
          const wasRecalculating = state.rules[index].recalculating;
          Object.assign(state.rules[index], hydrateRule(event.record));
          if (wasRecalculating && !event.record.recalculating && event.record.recalculation_error) {
            app.toasts.error(`Slug recalculation failed: ${event.record.recalculation_error}`);
          }
        } else {
          state.rules.push(hydrateRule(event.record));
        }
      }).then((unsubscribe) => {
        if (signal.aborted) unsubscribe();
        else unsubscribeRealtime = unsubscribe;
      }).catch((err) => {
        if (!signal.aborted) console.error("Unable to subscribe to slug rule updates.", err);
      });
    }
    signal.addEventListener("abort", () => unsubscribeRealtime?.(), { once: true });

    const ruleCard = (rule) => t.details(
      { className: "accordion slug-rule", open: !rule.id },
      t.summary(
        null,
        t.i({ className: "ri-links-line", ariaHidden: true }),
        () => {
          if (!rule._heading) return t.span({ className: "txt" }, "New rule");
          return [
            t.span({ className: "txt" }, rule._heading.collection),
            t.span({ className: "txt" }, ":"),
            ...rule._heading.inputs.map((field) => t.span({ className: "label" }, field)),
            t.span({ className: "txt" }, "→"),
            t.span({ className: "label" }, rule._heading.output),
          ];
        },
      ),
      t.form(
      { className: "slug-rule-form", onsubmit: (event) => { event.preventDefault(); save(rule); } },
      t.div({ className: "grid sm" },
        t.div({ className: "col-12 col-sm-6" }, t.div({ className: "field" },
          t.label(null, "Collection"),
          app.components.select({
            required: true,
            placeholder: "Select a collection…",
            options: () => state.collections.map((collection) => ({ value: collection.name, label: collection.name })),
            value: () => rule.collection_name,
            onchange: (options) => {
            rule.collection_name = options?.[0]?.value || "";
            rule.input_fields = [];
            rule.output_field = "";
          }}),
        )),
        t.div({ className: "col-12 col-sm-6" }, t.div({ className: "field" },
          t.label(null, "Input fields"),
          app.components.select({
            max: 99,
            required: true,
            placeholder: "Select input fields…",
            options: () => fieldOptions(state.collections, rule.collection_name),
            value: () => rule.input_fields,
            onchange: (options) => rule.input_fields = options.map((option) => option.value),
          }),
        )),
        t.div({ className: "col-12 col-sm-6" }, t.div({ className: "field" },
          t.label(null, "Output field"),
          app.components.select({
            required: true,
            placeholder: "Select an output field…",
            options: () => fieldOptions(state.collections, rule.collection_name).filter((field) => !field.value.includes(".") && ["text", "editor"].includes(field.type)),
            value: () => rule.output_field,
            onchange: (options) => rule.output_field = options?.[0]?.value || "",
          }),
        )),
        t.div({ className: "col-12 col-sm-6" }, t.div({ className: "field" },
          t.label(null, "Maximum length"),
          t.input({ type: "number", min: 1, required: true, value: () => rule.length, oninput: (event) => rule.length = event.target.value }),
        )),
      ),
      t.div({ className: "slug-rule-actions" },
        t.button({ type: "button", className: "btn danger outline", disabled: () => rule.recalculating, onclick: () => remove(rule) },
          t.i({ className: "ri-delete-bin-line" }), " Delete",
        ),
        t.div({ className: "slug-rule-actions-right" },
          t.button({ type: "button", className: "btn secondary", disabled: () => !rule.id || rule.recalculating, onclick: () => recalculate(rule) },
            t.i({ className: () => rule.recalculating ? "ri-loader-4-line" : "ri-refresh-line" }),
            () => rule.recalculating ? " Recalculating" : " Recalculate",
          ),
          t.button({ type: "submit", className: "btn", disabled: () => state.saving || rule.recalculating },
            t.i({ className: "ri-save-line" }), " Save",
          ),
        ),
      ),
      ),
    );

    return t.div({ className: "slug-settings" },
      () => state.loading ? t.div({ className: "slug-loading" }, t.span({ className: "loader" })) : null,
      () => !state.loading && !state.rules.length ? t.div({ className: "panel txt-center txt-hint" }, "No slug rules yet.") : null,
      () => state.rules.map(ruleCard),
      t.div({ className: "m-t-base" },
        t.button({ type: "button", className: "btn", onclick: () => state.rules.push(blankRule()) }, t.i({ className: "ri-add-line" }), " Add rule"),
      ),
    );
  },
};

(window.xpbPluginsPending ??= []).push(definition);
window.xpbPlugins?.flushPending();
