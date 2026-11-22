import { mount } from 'svelte';
import './themes/dark.css';
import './themes/light.css';
import './app.css';
import './ui/ui.css';
import './shell/shell.css';
import './help/help.css';
import App from './App.svelte';
import { initLang } from './lib/state.svelte.js';

// Язык — до первой отрисовки, чтобы не мигал русский.
initLang();
mount(App, { target: document.getElementById('app') });
