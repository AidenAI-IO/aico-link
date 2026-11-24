/**
 * Minimal DOM helpers for the settings UI.
 *
 * The module is import-safe: it touches no browser global until a function is
 * called, so the Node VM test harness can load it the same way the browser does.
 */

/**
 * Create an element.
 *
 * @param {string} tag
 * @param {object} [options]
 * @param {string} [options.class] - className, applied verbatim.
 * @param {string} [options.text] - textContent.
 * @param {Record<string, string>} [options.attrs] - plain attributes.
 * @param {Record<string, string>} [options.data] - `data-*` attributes, camelCase keys.
 * @param {Record<string, string>} [options.aria] - `aria-*` attributes, camelCase keys.
 * @param {Record<string, EventListener>} [options.on] - event listeners by type.
 * @param {Array<Node|string|null|undefined|false>} [children]
 * @returns {HTMLElement}
 */
export function el(tag, options = {}, children = []) {
  const node = document.createElement(tag);

  if (options.class) node.className = options.class;
  if (options.text != null) node.textContent = String(options.text);

  for (const [name, value] of Object.entries(options.attrs || {})) {
    if (value == null || value === false) continue;
    node.setAttribute(name, String(value));
  }
  for (const [name, value] of Object.entries(options.data || {})) {
    if (value == null || value === false) continue;
    node.setAttribute(`data-${dash(name)}`, String(value));
  }
  for (const [name, value] of Object.entries(options.aria || {})) {
    if (value == null || value === false) continue;
    node.setAttribute(`aria-${dash(name)}`, String(value));
  }
  for (const [type, handler] of Object.entries(options.on || {})) {
    node.addEventListener(type, handler);
  }

  append(node, children);
  return node;
}

/** Append children, skipping nullish and `false` entries from conditional lists. */
export function append(parent, children) {
  const list = Array.isArray(children) ? children : [children];
  for (const child of list) {
    if (child == null || child === false) continue;
    parent.appendChild(typeof child === 'string' ? document.createTextNode(child) : child);
  }
  return parent;
}

/** Remove every child of a node. */
export function clear(node) {
  while (node.firstChild) node.removeChild(node.firstChild);
  return node;
}

/** Replace the contents of a node with new children. */
export function replace(node, children) {
  clear(node);
  return append(node, children);
}

/** `inkSecondary` becomes `ink-secondary`. */
function dash(name) {
  return name.replace(/([a-z0-9])([A-Z])/g, '$1-$2').toLowerCase();
}

/**
 * Create an inline SVG element.
 *
 * @param {object} spec
 * @param {string} spec.viewBox
 * @param {Array<{tag: string, attrs: Record<string, string|number>, class?: string, children?: string[]}>} spec.shapes
 * @param {string} [spec.class]
 * @param {number} [spec.width]
 * @param {number} [spec.height]
 */
export function svg({ viewBox, shapes, class: className, width, height }) {
  const namespace = 'http://www.w3.org/2000/svg';
  const node = document.createElementNS(namespace, 'svg');
  node.setAttribute('viewBox', viewBox);
  node.setAttribute('fill', 'none');
  node.setAttribute('aria-hidden', 'true');
  node.setAttribute('focusable', 'false');
  if (className) node.setAttribute('class', className);
  if (width != null) node.setAttribute('width', String(width));
  if (height != null) node.setAttribute('height', String(height));
  for (const shape of shapes) {
    const child = document.createElementNS(namespace, shape.tag);
    // A class on a shape lets CSS paint it from a token, which is how a glyph
    // can take one colour inside a coloured tile and another as a standalone icon.
    if (shape.class) child.setAttribute('class', shape.class);
    for (const [name, value] of Object.entries(shape.attrs)) {
      child.setAttribute(name, String(value));
    }
    // A `g` may carry path data for children that inherit its transform and
    // stroke, which is how a 24px source glyph is placed inside a smaller box.
    for (const d of shape.children || []) {
      const path = document.createElementNS(namespace, 'path');
      path.setAttribute('d', d);
      child.appendChild(path);
    }
    node.appendChild(child);
  }
  return node;
}
