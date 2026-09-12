/* ArkGate UI 基础组件（Vue 3 全局构建，免 Node；在 app.js 之前加载）
 *
 * UiDrawer  —— 侧滑抽屉：承载复杂实体的分段编辑（替代窄弹窗，参照
 *              CLIProxyAPI Management Center 的 Sheet 模式）。
 * UiSwitch  —— 开关：替换「启用/停用」下拉，支持表格行内直接切换。
 */
"use strict";

const UiDrawer = {
  props: {
    open: Boolean,
    title: String,
    subtitle: String,
    width: { type: [Number, String], default: 640 },
  },
  emits: ["close"],
  computed: {
    w() {
      return typeof this.width === "number" ? this.width + "px" : this.width;
    },
  },
  methods: {
    onKey(e) {
      if (this.open && e.key === "Escape") this.$emit("close");
    },
  },
  mounted() { document.addEventListener("keydown", this.onKey); },
  unmounted() { document.removeEventListener("keydown", this.onKey); },
  template: `
  <teleport to="body">
    <transition name="drawer">
      <div v-if="open" class="drawer-mask" @click.self="$emit('close')">
        <div class="drawer" :style="{ width: w }" role="dialog" aria-modal="true">
          <div class="drawer-head">
            <div class="drawer-title">
              <h3>{{ title }}</h3>
              <span v-if="subtitle" class="drawer-sub">{{ subtitle }}</span>
            </div>
            <button class="drawer-close" title="关闭（Esc）" @click="$emit('close')">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M18 6 6 18M6 6l12 12"/></svg>
            </button>
          </div>
          <div class="drawer-body"><slot/></div>
          <div v-if="$slots.foot" class="drawer-foot"><slot name="foot"/></div>
        </div>
      </div>
    </transition>
  </teleport>`,
};

const UiSwitch = {
  props: {
    modelValue: Boolean,
    disabled: Boolean,
    size: { type: String, default: "" }, // "" | "sm"
  },
  emits: ["update:modelValue"],
  template: `
    <button type="button" class="ui-switch" :class="{ on: modelValue, sm: size==='sm' }"
      :disabled="disabled" role="switch" :aria-checked="modelValue ? 'true' : 'false'"
      @click="$emit('update:modelValue', !modelValue)"><span class="knob"></span></button>`,
};
