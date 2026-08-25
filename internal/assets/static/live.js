// live.js subscribes the page to server-sent events describing changes made
// by other browsers, and re-fetches just enough HTML through htmx to catch
// up. It is inert — does nothing at all — on a page whose <body> carries no
// data-live attribute, which is what lets layout.html load it unconditionally
// on every page.
(() => {
  const body = document.body;
  const url = body.dataset.live;
  if (!url) return;
  const collectionID = body.dataset.collection;

  // A per-tab id so the server can stamp events with their origin and this
  // tab can ignore its own echo: it already applied the change from the HTTP
  // response that caused it, and re-fetching would flicker and could clobber
  // an in-flight edit in this same tab.
  const TAB = crypto.randomUUID();
  document.body.addEventListener("htmx:configRequest", (e) => {
    e.detail.headers["X-Live-Origin"] = TAB;
  });

  // True once #items (or #collections) has an edit form open in it — a
  // wholesale swap while someone is mid-edit would destroy what they typed.
  // The itemForm fragment is a <form> inside the row, so its presence is the
  // signal, not any separate flag.
  function itemsHasOpenForm() {
    const items = document.getElementById("items");
    return items != null && items.querySelector("form") != null;
  }

  function itemRow(id) {
    return document.getElementById("item-" + id);
  }

  function fetchItem(id, swap) {
    htmx.ajax("GET", "/collections/" + collectionID + "/items/" + id, {
      target: swap.target,
      swap: swap.swap,
    });
  }

  // Both page types subscribe to this user's own topic, so each receives some
  // events meant for the other: a collection page sees the "collections"
  // events that move the index's progress counts, and would otherwise try to
  // swap a #collections element that only exists on the index. Every branch
  // below therefore checks that the thing it is about to update is actually
  // on this page, rather than assuming the stream only carries events this
  // page can use.
  function handle(ev) {
    switch (ev.kind) {
      // Only a text edit arrives as a per-item event now. Creating, ticking
      // and dragging can all move a row, so they publish "items" instead and
      // re-fetch the list; keeping edits per-row is what lets a swap land
      // without disturbing an edit form open elsewhere on the page.
      case "item":
        if (!collectionID) break;
        switch (ev.action) {
          case "updated": {
            const row = itemRow(ev.id);
            if (row && !row.querySelector("form")) {
              fetchItem(ev.id, { target: "#item-" + ev.id, swap: "outerHTML" });
            }
            break;
          }
          case "deleted":
            itemRow(ev.id)?.remove();
            break;
        }
        break;

      case "items":
        refetchItems();
        break;

      case "members":
        if (!collectionID) break;
        htmx.ajax("GET", "/collections/" + collectionID + "/members", {
          target: "#members",
          swap: "outerHTML",
        });
        break;

      case "collection":
        switch (ev.action) {
          case "renamed":
            // The name appears in the app bar, the page title and the
            // heading; reloading is cheaper than three coordinated swaps,
            // the same reasoning the rename handler's own HX-Refresh uses.
            location.reload();
            break;
          case "deleted":
            location.replace("/");
            break;
        }
        break;

      case "access":
        if (ev.action === "revoked" && String(ev.collection) === collectionID) {
          location.replace("/");
        }
        break;

      case "collections":
        // Only the index renders #collections. A collection page gets these
        // too, because it is subscribed to this user's topic for the sake of
        // "you were removed" notices, and simply has nothing to do with them.
        if (!document.getElementById("collections")) break;
        htmx.ajax("GET", "/collections", {
          target: "#collections",
          swap: "outerHTML",
        });
        break;
    }
  }

  // Re-fetch the whole item list. Used for any change that can move a row
  // rather than just alter one: a create, a tick (done items sort to the
  // bottom), or someone else's drag. The open-form guard is what stops a
  // colleague's tick from destroying an edit this tab has in progress; the
  // list stays stale only until that edit is saved or cancelled.
  function refetchItems() {
    if (!collectionID || itemsHasOpenForm()) return;
    htmx.ajax("GET", "/collections/" + collectionID + "/items", {
      target: "#items",
      swap: "outerHTML",
    });
  }

  // Re-fetch everything this page shows. Used on (re)connect so a missed
  // event — one dropped as a slow consumer, or one that happened while the
  // connection was down — can't leave the page permanently stale; the next
  // successful open just re-derives truth from scratch.
  function resync() {
    if (collectionID) {
      refetchItems();
      htmx.ajax("GET", "/collections/" + collectionID + "/members", {
        target: "#members",
        swap: "outerHTML",
      });
    } else {
      htmx.ajax("GET", "/collections", {
        target: "#collections",
        swap: "outerHTML",
      });
    }
  }

  const source = new EventSource(url);

  // onopen fires on the very first connect as well as every reconnect, but a
  // freshly loaded page is already up to date, so only a reconnect needs the
  // re-sync.
  let reconnected = false;
  source.onopen = () => {
    if (reconnected) resync();
    reconnected = true;
  };

  source.onmessage = (e) => {
    const ev = JSON.parse(e.data);
    if (ev.origin === TAB) return;
    handle(ev);
  };
})();
