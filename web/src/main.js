import { createApp } from "vue";
import "element-plus/es/components/message/style/css";
import "element-plus/es/components/message-box/style/css";
import "element-plus/es/components/notification/style/css";
import "element-plus/theme-chalk/dark/css-vars.css";

import App from "./App.vue";
import router from "./router/index.js";
import { initializeTheme } from "./stores/theme.js";
import "./styles/theme.css";
import "./styles/index.css";

initializeTheme();
const app = createApp(App).use(router);
// Keep the initial loading/recovery UI until authentication and route imports settle.
router.isReady().then(() => app.mount("#app")).catch(() => {
  window.dispatchEvent(new Event("icloud:startup-error"));
});
