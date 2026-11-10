let toastTimer = null;

function showToast(msg) {
  const el = document.getElementById("toast");
  if (!el) return;
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => el.classList.remove("show"), 2200);
}

async function copyTextFromElement(btn, elementId, label) {
  const el = document.getElementById(elementId);
  if (!el) return;
  const text = el.value !== undefined ? el.value : el.textContent;
  await copyRawText(btn, text, label);
}

async function copyRawText(btn, text, label) {
  try {
    await navigator.clipboard.writeText(text.trim());
    flashButton(btn, "Copied!");
    showToast((label || "Text") + " copied to clipboard");
  } catch (err) {
    console.error("Clipboard copy failed:", err);
    showToast("Copy failed — check browser clipboard permissions");
  }
}

// Converts any image URL (/posts/{id}/image) into a clean image/png Blob so
// Cmd+V works directly inside LinkedIn's composer across Chrome, Safari, Arc, and Firefox.
function imageURLToPngBlob(url) {
  return new Promise((resolve, reject) => {
    const img = new Image();
    img.crossOrigin = "anonymous";
    img.onload = () => {
      try {
        const canvas = document.createElement("canvas");
        canvas.width = img.naturalWidth;
        canvas.height = img.naturalHeight;
        const ctx = canvas.getContext("2d");
        ctx.drawImage(img, 0, 0);
        canvas.toBlob((blob) => {
          if (blob) resolve(blob);
          else reject(new Error("Canvas PNG conversion returned empty blob"));
        }, "image/png");
      } catch (e) {
        reject(e);
      }
    };
    img.onerror = () => reject(new Error("Failed loading image for clipboard"));
    img.src = url;
  });
}

async function copyImageToClipboard(btn, imageUrl) {
  if (!navigator.clipboard || typeof ClipboardItem === "undefined") {
    showToast("Image clipboard unsupported in this browser — use Download PNG");
    return;
  }
  try {
    // Passing Promise directly inside ClipboardItem preserves user gesture on macOS Safari & Chrome.
    const item = new ClipboardItem({
      "image/png": imageURLToPngBlob(imageUrl),
    });
    await navigator.clipboard.write([item]);
    flashButton(btn, "Image Copied!");
    showToast("Image copied! Press Cmd+V / Ctrl+V inside LinkedIn's post composer");
  } catch (err) {
    console.error("Copy image failed:", err);
    showToast("Could not copy image automatically — use Download PNG instead");
  }
}

function flashButton(btn, tempText) {
  if (!btn) return;
  const orig = btn.dataset.origLabel || btn.textContent;
  btn.dataset.origLabel = orig;
  btn.textContent = tempText;
  setTimeout(() => {
    btn.textContent = orig;
  }, 1800);
}
