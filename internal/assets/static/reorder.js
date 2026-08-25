// reorder.js makes the item list drag-sortable and persists the new order.
//
// It is inert on any page without an #items list, so layout.html can load it
// unconditionally the way it loads live.js.
(() => {
  // Sortable rebuilds its bindings against a specific <ul>. Every swap that
  // replaces #items — create, toggle, reorder, a live re-sync — hands us a
  // brand new element, so binding once at load would silently stop working
  // after the first tick. Re-binding on htmx:afterSwap is what keeps dragging
  // alive for the life of the page.
  let sortable = null;

  function bind() {
    const list = document.getElementById("items");
    if (!list) return;

    if (sortable) sortable.destroy();

    const collectionID = list.dataset.collectionId || list.dataset.collection;

    sortable = new Sortable(list, {
      handle: ".drag-handle",
      // A done item's position is decided by the sort, not by hand, so it
      // must not be draggable — otherwise it would animate to wherever it was
      // dropped and then snap back when the server's ordering came through.
      filter: ".item.done",
      animation: 150,
      // Dropping an undone item below the done ones has the same snap-back
      // problem in reverse, so refuse the move rather than let it happen and
      // undo it.
      onMove: (evt) => !evt.related.classList.contains("done"),
      onEnd: () => save(list, collectionID),
    });
  }

  function save(list, collectionID) {
    const ids = Array.from(list.querySelectorAll(".item"))
      .map((li) => li.dataset.itemId)
      .filter(Boolean);
    if (!ids.length) return;

    // Posting through htmx rather than fetch keeps this on the same path as
    // every other mutation: it picks up the HX-Request header the CSRF guard
    // requires and the X-Live-Origin header live.js adds, so this tab ignores
    // the echo of its own reorder.
    htmx.ajax("POST", "/collections/" + collectionID + "/items/reorder", {
      target: "#items",
      swap: "outerHTML",
      values: { order: ids.join(",") },
    });
  }

  bind();
  document.body.addEventListener("htmx:afterSwap", (e) => {
    if (e.target && (e.target.id === "items" || e.target.querySelector?.("#items"))) {
      bind();
    }
  });
})();
