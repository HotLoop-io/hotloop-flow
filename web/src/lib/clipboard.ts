/** Puts text on the clipboard. Falls back to a selection copy where the page is not a secure context. */
export async function copyText(text: string, from?: HTMLTextAreaElement): Promise<boolean> {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* denied: try the old way */
  }
  if (!from) return false;
  from.select();
  try {
    return document.execCommand('copy');
  } catch {
    return false;
  }
}
