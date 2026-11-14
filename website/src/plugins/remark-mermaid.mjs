function escapeHtml(text) {
  return text
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

function transformNode(node) {
  if (!node || typeof node !== 'object') return;
  if (Array.isArray(node.children)) {
    for (let i = 0; i < node.children.length; i++) {
      const child = node.children[i];
      if (child.type === 'code' && child.lang === 'mermaid') {
        const escaped = escapeHtml(child.value || '');
        node.children[i] = {
          type: 'html',
          value: `<div class="cp-mermaid-wrap"><pre class="mermaid" data-mermaid-src="${escaped}">${escaped}</pre></div>`,
        };
      } else {
        transformNode(child);
      }
    }
  }
}

export function remarkMermaid() {
  return (tree) => {
    transformNode(tree);
  };
}
