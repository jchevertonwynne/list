// cover.js turns a pasted or picked photo into a JPEG cover upload. It is
// inert on any page without a #cover-form — every page except a collection's
// — so layout.html can load it unconditionally the way it loads live.js and
// reorder.js.
(() => {
  const collectionID = document.body.dataset.collection;
  const form = document.getElementById("cover-form");
  const input = document.getElementById("cover-input");
  const pickButton = document.getElementById("cover-pick");
  if (!collectionID || !form || !input) return;

  // Mirrors maxCoverBytes in routes.go. There is no build step sharing a
  // constant between Go and JS, so this only lets quality-stepping stop
  // before the request goes out — the server enforces the real cap
  // regardless, via MaxBytesReader, so this is a client-side courtesy, not
  // the boundary.
  const MAX_BYTES = 2 << 20;
  const MAX_EDGE = 1600;

  if (pickButton) {
    pickButton.addEventListener("click", () => input.click());
  }

  input.addEventListener("change", () => {
    const file = input.files && input.files[0];
    // Cleared so picking the same file twice in a row still fires "change" —
    // otherwise a second selection of an unchanged file is silently a no-op.
    input.value = "";
    if (file) upload(file);
  });

  // Gated on the clipboard actually carrying an image: the collection page
  // has three text inputs and a textarea, and pasting text into any of them
  // must not be intercepted here. A clipboard holding plain text — including
  // a URL from "Copy image address" — is deliberately left alone: fetching a
  // remote URL would break this app's "no external requests" property for
  // the sake of a shortcut.
  document.addEventListener("paste", (e) => {
    const items = e.clipboardData && e.clipboardData.items;
    if (!items) return;
    for (const item of items) {
      if (item.kind === "file" && item.type.startsWith("image/")) {
        e.preventDefault();
        const file = item.getAsFile();
        if (file) upload(file);
        return;
      }
    }
  });

  function encode(canvas, quality) {
    return new Promise((resolve) => canvas.toBlob(resolve, "image/jpeg", quality));
  }

  async function upload(file) {
    const url = URL.createObjectURL(file);
    let blob;
    try {
      // new Image() + decode(), not createImageBitmap: createImageBitmap's
      // imageOrientation defaults to "none", which leaves an EXIF-rotated
      // phone photo sideways. Decoding through <img> applies the orientation
      // tag, so naturalWidth/naturalHeight already describe the rotated
      // image — load-bearing now that the server strips EXIF entirely, since
      // the rotation has to be baked into the pixels here or it is sideways
      // for good.
      const img = new Image();
      img.src = url;
      await img.decode();

      const scale = Math.min(1, MAX_EDGE / Math.max(img.naturalWidth, img.naturalHeight));
      const canvas = document.createElement("canvas");
      canvas.width = Math.round(img.naturalWidth * scale);
      canvas.height = Math.round(img.naturalHeight * scale);
      canvas.getContext("2d").drawImage(img, 0, 0, canvas.width, canvas.height);

      // Always re-encode to JPEG, even when the original was already small
      // enough to fit the cap. Passing untouched bytes through "because they
      // already fit" is exactly the optimisation that would quietly
      // reintroduce EXIF client-side, and image/jpeg is the one format every
      // browser's toBlob supports, so there is no fallback dance to detect.
      // Stepping quality down is what keeps the cap client-side rather than
      // relying on the server's 413 as the only enforcement.
      blob = await encode(canvas, 0.9);
      for (let q = 0.8; blob.size > MAX_BYTES && q > 0.1; q -= 0.1) {
        blob = await encode(canvas, q);
      }
    } finally {
      URL.revokeObjectURL(url);
    }
    // Still oversized even at the bottom of the quality range: let the
    // server's own cap answer with a 413 rather than inventing a client-side
    // error message for the same condition.
    if (blob.size > MAX_BYTES) return;

    // htmx 2.0.10 reads hx-encoding off the *request element*, not off this
    // context object — a bare {values:{cover: blob}} call has no source, so
    // it falls back to document.body, misses the multipart branch entirely,
    // and urlencodes the blob to the literal string "[object Blob]": a
    // silent, 13-byte "success". Passing the hidden form as source is what
    // makes htmx pick the multipart path instead. Going through htmx at all,
    // rather than fetch, is also what picks up the HX-Request header the
    // CSRF guard requires and the X-Live-Origin header live.js adds — see
    // reorder.js for the same reasoning.
    htmx.ajax("PUT", "/collections/" + collectionID + "/cover", {
      source: form,
      values: { cover: blob },
    });
  }
})();
