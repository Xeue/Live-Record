/*
 * host.jsx — ExtendScript run inside Premiere Pro.
 *
 * Everything here exists to answer one question: can a growing clip be made to
 * re-read its media without switching focus away from Premiere and back?
 *
 * Verified present in Premiere 26.2 by inspecting the shipped binary:
 *   projectItem.refreshMedia()      — posts a DIFFERENT internal message from
 *                                     the Source Monitor's Force Media Refresh
 *                                     button, so it is worth testing separately
 *   projectItem.changeMediaPath()   — rebuilds the media link entirely; there is
 *                                     precedent that this works where
 *                                     refreshMedia() does not (image sequences)
 *   projectItem.canChangeMediaPath()
 *   projectItem.attachProxy()
 *   projectItem.setOffline()
 *   app.project.pauseGrowing()
 *
 * NOT present, despite appearing in some write-ups: setOverrideFilePath().
 */

// ---------------------------------------------------------------------------
// helpers

function lrEsc(s) {
    if (s === null || s === undefined) return "";
    return String(s).replace(/\\/g, "\\\\").replace(/"/g, '\\"');
}

// Walk the whole project tree, not just the root, so clips in bins are found.
function lrCollect(item, out, depth) {
    if (!item || depth > 8) return;
    var n = 0;
    try { n = item.children ? item.children.numItems : 0; } catch (e) { n = 0; }
    for (var i = 0; i < n; i++) {
        var c = item.children[i];
        try {
            if (c.type === ProjectItemType.CLIP || c.type === ProjectItemType.FILE) {
                out.push(c);
            }
        } catch (e) {}
        lrCollect(c, out, depth + 1);
    }
}

function lrItems() {
    var out = [];
    try { lrCollect(app.project.rootItem, out, 0); } catch (e) {}
    return out;
}

function lrFindByPath(path) {
    var items = lrItems();
    for (var i = 0; i < items.length; i++) {
        try { if (items[i].getMediaPath() === path) return items[i]; } catch (e) {}
    }
    return null;
}

// The clip's current end point, in seconds. This is the number that should grow
// when a refresh succeeds.
function lrDuration(item) {
    try {
        var o = item.getOutPoint();
        if (o && o.seconds !== undefined) return Number(o.seconds);
    } catch (e) {}
    try {
        if (item.duration && item.duration.seconds !== undefined) {
            return Number(item.duration.seconds);
        }
    } catch (e) {}
    return -1;
}

// ---------------------------------------------------------------------------
// called from the panel

function lrList() {
    var items = lrItems(), parts = [];
    for (var i = 0; i < items.length; i++) {
        var p = "";
        try { p = items[i].getMediaPath(); } catch (e) {}
        if (!p) continue;
        parts.push('{"name":"' + lrEsc(items[i].name) + '","path":"' + lrEsc(p) +
                   '","duration":' + lrDuration(items[i]) + '}');
    }
    return "[" + parts.join(",") + "]";
}

function lrGetDuration(path) {
    var it = lrFindByPath(path);
    if (!it) return "-1";
    return String(lrDuration(it));
}

// Technique 1 — the documented refresh. Cheapest thing to try.
function lrRefreshMedia(path) {
    var it = lrFindByPath(path);
    if (!it) return '{"ok":false,"error":"clip not found in project"}';
    var before = lrDuration(it), ret = null, err = "";
    try { ret = it.refreshMedia(); } catch (e) { err = String(e); }
    var after = lrDuration(it);
    return '{"ok":' + (err === "") + ',"before":' + before + ',"after":' + after +
           ',"returned":"' + lrEsc(ret) + '","error":"' + lrEsc(err) + '"}';
}

// Technique 2 — relink the clip to an identical path. altPath must be a second
// name for the SAME bytes (a hardlink), so the media never actually changes;
// only Premiere's link to it is rebuilt, which forces a genuine re-read.
function lrChangeMediaPath(path, altPath) {
    var it = lrFindByPath(path);
    if (!it) return '{"ok":false,"error":"clip not found in project"}';
    var before = lrDuration(it), err = "", can = "unknown", ret = null;
    try { can = String(it.canChangeMediaPath(altPath)); } catch (e) {}
    try { ret = it.changeMediaPath(altPath, 1); } catch (e) { err = String(e); }
    var after = lrDuration(it);
    return '{"ok":' + (err === "") + ',"before":' + before + ',"after":' + after +
           ',"can":"' + lrEsc(can) + '","returned":"' + lrEsc(ret) +
           '","error":"' + lrEsc(err) + '","now":"' + lrEsc(itPath(it)) + '"}';
}

function itPath(it) {
    try { return it.getMediaPath(); } catch (e) { return ""; }
}

// Technique 3 — nudge the growing-file manager. Expected to be a no-op for a
// clip Premiere never classified as growing, which is itself informative.
function lrPauseGrowing(pauseThenResume) {
    var err = "";
    try {
        app.project.pauseGrowing(1);
        if (pauseThenResume) app.project.pauseGrowing(0);
    } catch (e) { err = String(e); }
    return '{"ok":' + (err === "") + ',"error":"' + lrEsc(err) + '"}';
}

// Technique 4 — the documented offline/online round trip. Heavy: the clip goes
// visibly offline first, so this is a last resort rather than something to run
// on a timer during a show.
function lrOfflineOnline(path) {
    var it = lrFindByPath(path);
    if (!it) return '{"ok":false,"error":"clip not found in project"}';
    var before = lrDuration(it), err = "";
    try {
        it.setOffline();
        it.refreshMedia();
    } catch (e) { err = String(e); }
    var after = lrDuration(it);
    return '{"ok":' + (err === "") + ',"before":' + before + ',"after":' + after +
           ',"error":"' + lrEsc(err) + '"}';
}

// What the host actually supports, so the panel can report facts not guesses.
function lrCapabilities() {
    var items = lrItems();
    var probe = items.length ? items[0] : null;
    function has(o, m) { try { return typeof o[m] === "function"; } catch (e) { return false; } }
    return '{"version":"' + lrEsc(app.version) +
           '","clips":' + items.length +
           ',"refreshMedia":' + (probe ? has(probe, "refreshMedia") : false) +
           ',"changeMediaPath":' + (probe ? has(probe, "changeMediaPath") : false) +
           ',"canChangeMediaPath":' + (probe ? has(probe, "canChangeMediaPath") : false) +
           ',"attachProxy":' + (probe ? has(probe, "attachProxy") : false) +
           ',"setOffline":' + (probe ? has(probe, "setOffline") : false) +
           ',"pauseGrowing":' + has(app.project, "pauseGrowing") + '}';
}

// ---------------------------------------------------------------------------
// bulk refresh — the working technique, applied to every growing clip at once
//
// refreshMedia() is the one that moved the duration in testing, so this is the
// production path. Everything above is kept as a diagnostic bench.

function lrRefreshMany(pathsCSV) {
    var paths = pathsCSV.length ? pathsCSV.split("\n") : [];
    var parts = [];
    for (var i = 0; i < paths.length; i++) {
        var p = paths[i];
        if (!p) continue;
        var it = lrFindByPath(p);
        if (!it) {
            parts.push('{"path":"' + lrEsc(p) + '","found":false}');
            continue;
        }
        var before = lrDuration(it), err = "";
        try { it.refreshMedia(); } catch (e) { err = String(e); }
        var after = lrDuration(it);
        parts.push('{"path":"' + lrEsc(p) + '","found":true,"name":"' + lrEsc(it.name) +
                   '","before":' + before + ',"after":' + after +
                   ',"error":"' + lrEsc(err) + '"}');
    }
    return "[" + parts.join(",") + "]";
}
