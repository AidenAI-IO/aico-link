"""Low-level keyboard and persistent contact input for the browser simulator.

These primitives deliberately do not focus fields, fill values, or choose targets.
The Agent retains its text-input engine and observes screenshots after each action.
"""
from __future__ import annotations

from typing import Any

# Match MobileGym's simInput touch/pointer events, but keep the original target
# and contact across requests. Coordinates are normalized against the viewport,
# not divided by a guessed device-pixel ratio.
_CONTACT = r"""async ({operation, point}) => {
    const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
    const position = p => ({x: p.x * innerWidth / 1000, y: p.y * innerHeight / 1000});
    const dispatch = (contact, phase) => {
        const {target, x, y} = contact;
        const ended = phase === 'up' || phase === 'cancel';
        const touch = new Touch({identifier: 1, target, clientX: x, clientY: y,
            pageX: x + scrollX, pageY: y + scrollY, screenX: x, screenY: y});
        target.dispatchEvent(new TouchEvent('touch' + ({down:'start',move:'move',up:'end',cancel:'cancel'}[phase]), {
            bubbles:true, cancelable:true, composed:true,
            touches: ended ? [] : [touch], targetTouches: ended ? [] : [touch], changedTouches:[touch]
        }));
        target.dispatchEvent(new PointerEvent('pointer' + phase, {
            bubbles:true, cancelable:true, composed:true, pointerId:1,
            pointerType:'touch', isPrimary:true, clientX:x, clientY:y,
            buttons:ended ? 0 : 1, pressure:ended ? 0 : 0.5
        }));
    };
    const move = async (contact, end, ms) => {
        const start = {x:contact.x, y:contact.y};
        for (let i=1; i<=10; i++) {
            contact.x = start.x + (end.x-start.x)*i/10;
            contact.y = start.y + (end.y-start.y)*i/10;
            dispatch(contact, 'move');
            await sleep(ms/10);
        }
    };
    let contact = window.__AIDEN_CONTACT__;
    if (operation === 'cancel') {
        if (contact) dispatch(contact, 'cancel');
        delete window.__AIDEN_CONTACT__;
        return;
    }
    if (operation === 'drag_start') {
        if (contact) throw new Error('drag contact already active; release it first');
        const start = position(point);
        contact = {target:document.elementFromPoint(start.x,start.y) || document.body, ...start};
        window.__AIDEN_CONTACT__ = contact;
        dispatch(contact, 'down');
        await sleep(500);
        // Same bounded 200-unit activation movement as the Go MNK provider.
        const candidates = [
            {room:1000-point.x, x:200, y:0}, {room:1000-point.y, x:0, y:200},
            {room:point.x, x:-200, y:0}, {room:point.y, x:0, y:-200}
        ];
        const direction = candidates.reduce((best, item) => item.room > best.room ? item : best);
        await move(contact, position({x:point.x+direction.x,y:point.y+direction.y}), 400);
        return;
    }
    if (!contact) throw new Error('no active drag contact; call drag_start first');
    await move(contact, position(point), 200);
    if (operation === 'drag_release') {
        await sleep(200);
        dispatch(contact, 'up');
        delete window.__AIDEN_CONTACT__;
    }
}"""


class SimulatorInput:
    def __init__(self, env: Any):
        self.env = env
        self.contact_active = False

    @property
    def page(self) -> Any:
        page = getattr(self.env, "page", None)
        if page is None:
            raise ValueError("MobileGym input requires a browser page")
        return page

    async def text(self, text: str) -> None:
        # Native key events preserve editing/undo semantics needed by the probe.
        await self.page.keyboard.type(text)

    async def keypress(self, keys: list[str]) -> None:
        aliases = {
            "ctrl": "Control", "control": "Control", "lctrl": "Control", "rctrl": "Control",
            "meta": "Control", "cmd": "Control", "super": "Control", "win": "Control",
            "shift": "Shift", "lshift": "Shift", "rshift": "Shift", "alt": "Alt",
            "enter": "Enter", "return": "Enter", "tab": "Tab", "space": "Space",
            "backspace": "Backspace", "delete": "Delete", "escape": "Escape", "esc": "Escape",
            "left": "ArrowLeft", "right": "ArrowRight", "up": "ArrowUp", "down": "ArrowDown",
            "home": "Home", "end": "End", "pageup": "PageUp", "pagedown": "PageDown",
        }
        chord = []
        for key in keys:
            normalized = key.strip().lower()
            if normalized in aliases:
                chord.append(aliases[normalized])
            elif len(normalized) == 1 and normalized.isascii() and normalized.isprintable():
                chord.append(normalized)
            else:
                raise ValueError(f"unsupported simulator keyboard key: {key!r}")
        await self.page.keyboard.press("+".join(chord))

    async def contact(self, operation: str, point: dict[str, float]) -> None:
        if operation == "drag_start" and self.contact_active:
            raise ValueError("drag contact already active; release it first")
        if operation != "drag_start" and not self.contact_active:
            raise ValueError("no active drag contact; call drag_start first")
        try:
            await self.page.evaluate(_CONTACT, {"operation": operation, "point": point})
            self.contact_active = operation != "drag_release"
        except BaseException:
            await self.release()
            raise

    async def release(self) -> None:
        try:
            await self.page.evaluate(_CONTACT, {"operation": "cancel", "point": None})
        finally:
            self.contact_active = False
