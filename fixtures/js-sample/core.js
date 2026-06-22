// Minimal event-driven core used as a js-events lens fixture.
export class Core {
  constructor() {
    this.listeners = {};
    this.actions = {};
  }

  on(event, fn) {
    (this.listeners[event] = this.listeners[event] || []).push(fn);
  }

  emit(event, payload) {
    (this.listeners[event] || []).forEach((fn) => fn(payload));
  }

  registerAction(name, fn) {
    this.actions[name] = fn;
  }
}

export function bootstrap(core) {
  // emit/subscribe pair on the same channel so the lens shows both sides.
  core.on("state:changed", (s) => render(s));
  core.emit("state:changed", { ready: true });
  core.registerAction("move", () => {});
}

function render(state) {
  return state;
}
