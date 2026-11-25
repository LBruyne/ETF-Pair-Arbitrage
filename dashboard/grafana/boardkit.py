"""Building blocks of the ETF-Pair boards: where a board is filed, colours,
panel and variable constructors, and the output. Every query is PostgreSQL,
for the data source a board names in reset_ids() (Combo Spread: etfpair;
Live Monitor: etfpair-trade); panel ids count up from it."""
import json, os, re

HERE = os.path.dirname(os.path.abspath(__file__))
PG = {"type": "grafana-postgresql-datasource", "uid": "etfpair"}             # the collector's tables, role etfpair_reader
TRADE = {"type": "grafana-postgresql-datasource", "uid": "etfpair-trade"}  # the trading program's schema trade, role etfpair_trade_reader
TIMEZONE = "Etc/GMT-8"  # IANA signs Etc zones backwards: this is UTC+8

# A board's file path under dashboards/staging/ is its place in Grafana: the
# directories are its folders. Its address is /dashboards/<folder path>/<title>
# in lower case with dashes (/dashboards/arbitrage/etf-pair/combo-spread).
# Grafana itself serves /d/<uid>/<slug> and a uid holds no slash, so nginx
# redirects the address to it (board_paths_conf).
FOLDER = "Arbitrage/ETF-Pair"
UID_PREFIX = "arb-etf-pair"
BOARDS = {"combo-spread": "Combo Spread", "live-monitor": "Live Monitor"}
STAGING = os.path.join(HERE, "dashboards", "staging")

# The hues of the arbitrage boards next door (dashboard/SPEC.md section 6):
# blue and orange for the two sides of a pair, purple for a perp quantity,
# teal for an entry line; neutrals for references. TEXT is the theme's text
# colour, light on the dark theme and dark on the light one.
BLUE, ORANGE, PURPLE, TEAL, TEXT = "#2a78d6", "#d95926", "#9b59d0", "#4fc3d9", "text"
GREY, LIGHT_GREY = "#898781", "#a8a69e"
# The bands behind every chart, one hue per phase of the US equity day:
# amber, pink and blue pass validate_palette.js for all pairs on both
# surfaces; the regular session is a neutral, a closed market has no band.
PHASES = {"Pre-Market": "#b88a0a", "Regular": LIGHT_GREY, "After-Hours": "#d55181", "Overnight": "#4f8fe6"}
GOOD, WARN, BAD = "#0ca30c", "#fab219", "#d03b3b"
BPS = "suffix: bps"
LEGEND_PX, AXIS_PX = 400, 60  # a chart's legend table and its value axis
# The dashboard JSON schema the boards are written in: Grafana 13 migrates an older board on load, and its step to 42 adds
# tooltip: true to every series hidden from the plot, which would take tooltip_only series out of the tooltip.
SCHEMA_VERSION = 42
_id = 0
_ds = PG   # the data source of the board being built (reset_ids)


def sql(text, **parts):
    """Fills @NAME@ placeholders; every one must be filled."""
    for k, v in parts.items():
        text = text.replace("@" + k + "@", v)
    assert not re.search(r"@[A-Z_]+@", text), text
    return text.strip()


def nid():
    global _id
    _id += 1
    return _id


def reset_ids(datasource=PG):
    """Starts a board: panel ids from 1, every query on datasource."""
    global _id, _ds
    _id, _ds = 0, datasource


def target(q, ref="A"):
    return {"refId": ref, "datasource": dict(_ds), "format": "table", "rawQuery": True, "editorMode": "code", "rawSql": q}


# Grafana's own data source that hands a panel another panel's query result: the page runs a query once for several panels.
DASHBOARD = {"type": "datasource", "uid": "-- Dashboard --"}


def panel(kind, title, q, x, y, w, h, description="", unit=None, decimals=None, options=None, overrides=None, custom=None, color=None,
          thresholds=None, mappings=None, no_value="–", source=None, transformations=None):
    """A panel that runs q, or, given source (a panel), that shows source's result: Grafana does not run the query again
    for it, and it takes source's frame as the query returned it (withTransforms false), through its own transformations.
    The source must sit in an expanded row: Grafana runs a source in a collapsed row twice on load."""
    defaults = {"color": color or {"mode": "fixed", "fixedColor": "text"}, "custom": custom or {}, "mappings": mappings or [], "noValue": no_value}
    if unit:
        defaults["unit"] = unit
    if decimals is not None:
        defaults["decimals"] = decimals
    if thresholds:
        defaults["thresholds"] = thresholds
    p = {"id": nid(), "type": kind, "title": title, "description": description, "gridPos": {"x": x, "y": y, "w": w, "h": h}, "datasource": dict(_ds),
         "targets": [target(q)], "fieldConfig": {"defaults": defaults, "overrides": overrides or []}, "options": options or {}}
    if source is not None:
        p["datasource"] = DASHBOARD
        p["targets"] = [{"refId": "A", "datasource": DASHBOARD, "panelId": source["id"], "withTransforms": False}]
    if transformations:
        p["transformations"] = transformations
    return p


def keep_fields(names):
    """Transformation: only these fields of the frame, in the frame's order."""
    return {"id": "filterFieldsByName", "options": {"include": {"names": list(names)}}}


def organize(order, rename=None, exclude=()):
    """Transformation: the fields in this order (names as they reach it, before its own renaming), renamed, some hidden."""
    return {"id": "organize", "options": {"indexByName": {name: i for i, name in enumerate(order)}, "renameByName": dict(rename or {}),
                                          "excludeByName": {name: True for name in exclude}}}


def copy_field(name, alias):
    """Transformation: a copy of the field name under alias, made in the browser (a row's last value of that one field), so
    that the frame need not carry it twice."""
    return {"id": "calculateField", "options": {"mode": "reduceRow", "reduce": {"include": [name], "reducer": "lastNotNull"}, "alias": alias,
                                                 "replaceFields": False}}


def calc(alias, left, operator, right):
    """Transformation: a field alias = left operator right, made in the browser row by row (Grafana's binary operation);
    left and right are field names or numbers. Grafana 13 reads a NULL operand as 0 (Live Monitor round 2: the lines of a
    break row, all NULL, came out 0), so a result over rows that break the lines must be computed in SQL instead."""
    def operand(x):
        return {"fixed": str(x)} if isinstance(x, (int, float)) else {"matcher": {"id": "byName", "options": x}}
    return {"id": "calculateField", "options": {"mode": "binary", "binary": {"left": operand(left), "operator": operator, "right": operand(right)},
                                                 "alias": alias, "replaceFields": False}}


def rows_with(field):
    """Transformation: only the rows at which field has a value."""
    return {"id": "filterByValue", "options": {"filters": [{"fieldName": field, "config": {"id": "isNotNull", "options": {}}}], "type": "include",
                                               "match": "any"}}


def group_by(keys, reduce):
    """Transformation: a row per distinct value of keys, with each field of reduce ({field: [reducer, ...]}) reduced over the
    group; Grafana names a result "<field> (<reducer>)" and keeps the groups in the order they first occur."""
    fields = {k: {"operation": "groupby", "aggregations": []} for k in keys}
    fields.update({f: {"operation": "aggregate", "aggregations": list(r)} for f, r in reduce.items()})
    return {"id": "groupBy", "options": {"fields": fields}}


def sort_by(field):
    return {"id": "sortBy", "options": {"fields": {}, "sort": [{"field": field, "desc": False}]}}


def row(title, y):
    return {"id": nid(), "type": "row", "title": title, "collapsed": False, "gridPos": {"x": 0, "y": y, "w": 24, "h": 1}, "panels": []}


def named(name, props):
    """An override for the one field with this name. Grafana does not fill
    variables in a matcher, so a name that holds one (${bull} Price) is matched
    by a pattern with the variable left open."""
    properties = [{"id": k, "value": v} for k, v in props.items()]
    if "${" not in name:
        return {"matcher": {"id": "byName", "options": name}, "properties": properties}
    pattern = "^" + ".+".join(re.escape(part) for part in re.split(r"\$\{[^}]+\}", name)) + "$"
    return {"matcher": {"id": "byRegexp", "options": pattern}, "properties": properties}


PLAIN = {"mode": "absolute", "steps": [{"color": "text", "value": None}]}
SIGNED = {"mode": "absolute", "steps": [{"color": BAD, "value": None}, {"color": "text", "value": 0}, {"color": GOOD, "value": 0.000001}]}
NO_FIGURE = {"type": "special", "options": {"match": "null", "result": {"text": "–", "color": GREY, "index": 0}}}


def stat_strip(title, q, figures, x, y, w, h, intro="", source=None, columns=None, title_size=13, value_size=20):
    """A row of figures in one stat panel, each with its name above it.
    q returns one row with one column per figure; figures is a list of
    (column, about, props): about is the figure's line in the panel's
    description, props its field properties (unit, decimals, thresholds;
    displayName for the name shown, which may hold a variable, as a column
    shared with other panels does not). Names are 13 px and figures 20 px:
    at about 840 px of content (a 1280 px screen with the side menu open)
    seven figures and "Long · Partial" stay on one line, which they do not
    at 24 px; a strip whose names or figures are longer takes title_size or
    value_size (an 880 px window in normal mode leaves about 790 px). With
    source the strip shows those of source's columns; a source whose query
    returns more columns than its own figures keeps its own (columns, in
    the frame's order)."""
    opts = {"reduceOptions": {"calcs": ["lastNotNull"], "fields": "/.*/", "values": False}, "colorMode": "value", "graphMode": "none",
            "textMode": "value_and_name", "justifyMode": "center", "wideLayout": False, "text": {"titleSize": title_size, "valueSize": value_size},
            "showPercentChange": False, "orientation": "vertical"}
    about = "\n".join("- **%s**: %s" % ((props or {}).get("displayName", name), text) for name, text, props in figures if text)
    keep = [keep_fields(columns or [name for name, _, _ in figures])] if source is not None or columns else None
    return panel("stat", title, q, x, y, w, h, description="\n\n".join(p for p in (intro, about) if p), options=opts, color={"mode": "thresholds"},
                 thresholds=PLAIN, mappings=[NO_FIGURE], overrides=distinct(named(name, props) for name, _, props in figures if props),
                 source=source, transformations=keep)


def distinct(overrides):
    """Overrides without repeats: two names that differ only in a variable give the same pattern."""
    out = []
    for o in overrides:
        if o not in out:
            out.append(o)
    return out


def col(name, px=None, **props):
    """A table column override: px sets the width; custom.* is spelled custom_*."""
    out = {k.replace("custom_", "custom."): v for k, v in props.items()}
    if px:
        out["custom.width"] = px
    return named(name, out)


def table(title, q, x, y, w, h, description="", overrides=None, source=None, transformations=None):
    opts = {"showHeader": True, "cellHeight": "sm", "footer": {"show": False}}
    custom = {"align": "auto", "cellOptions": {"type": "auto"}, "filterable": False, "inspect": False, "minWidth": 60, "wrapHeaderText": True}
    return panel("table", title, q, x, y, w, h, description=description, options=opts, overrides=distinct(overrides or []), custom=custom,
                 source=source, transformations=transformations)


def series(name, color, width=1, dash=None, fill_to=None, fill=0, legend=True, tooltip=True):
    """One line of a time series panel: its colour, width, dash pattern and,
    with fill_to, a translucent band down to another series. legend=False
    keeps a twin line (the lower of a ± pair) out of the legend, tooltip=False
    keeps a line out of the tooltip (where a tooltip_only twin may stand in
    for it)."""
    props = {"color": {"mode": "fixed", "fixedColor": color}, "custom.lineWidth": width}
    if dash:
        props["custom.lineStyle"] = {"fill": "dash", "dash": dash}
    if fill_to:
        props["custom.fillBelowTo"] = fill_to
        props["custom.fillOpacity"] = fill
    if not (legend and tooltip):
        props["custom.hideFrom"] = {"legend": not legend, "tooltip": not tooltip, "viz": False}
    return named(name, props)


def code_texts(codes, colors=None):
    """A value mapping of the numbers in codes ({number: text}) to their text, coloured from colors ({text: colour}), else grey."""
    return {"type": "value", "options": {str(code): {"text": text, "color": (colors or {}).get(text, GREY), "index": i}
                                         for i, (code, text) in enumerate(codes.items())}}


def tooltip_only(name, color, mapping=None, props=None, shown=None):
    """A series that is neither drawn nor in the legend: only the tooltip shows its value at each point (hideFrom.viz:
    no line, no scale, no hover dot; the board's schemaVersion is 42, as Grafana 13 migrates an older board's viz-hidden
    series out of the tooltip as well). mapping (code_texts) turns the numbers a query returns for "no value here" into the
    text the tooltip shows: Grafana's tooltip shows a nearby point's value in place of a NULL, so a point without a value
    carries a code. props adds field properties (unit, decimals); shown is the name the tooltip shows. A twin that stands in
    for a drawn line is shown under that line's name and a zero-width space: Grafana's legend, isolating a line, keeps every
    series whose name is that of a series out of the legend, so a twin of the same name would keep its line drawn."""
    props = dict(props or {}, **{"color": {"mode": "fixed", "fixedColor": color}, "custom.hideFrom": {"legend": True, "tooltip": False, "viz": True}})
    if mapping:
        props["mappings"] = [mapping]
    if shown:
        props["displayName"] = shown
    return named(name, props)


def timeseries(title, q, x, y, w, h, lines, description="", decimals=1, zero_line=False, right_axis=False, source=None, transformations=None,
               soft_min=None, calcs=("lastNotNull", "min", "max")):
    """q returns a time column and one numeric column per line; a NULL breaks
    the line, so a minute without data shows as a gap. lines are series().
    The unit is named in the title and the axis shows bare numbers. The
    legend is a table beside the plot, which leaves the plot its full height;
    it and the axis have a fixed width, so the time axes of the charts
    stacked on a board line up; calcs are its columns (Grafana's reducers,
    the same for every line). zero_line draws a dashed line at zero.
    right_axis says that one of the lines has an axis of its own on the right:
    the legend gives up that axis's width, less the 8 px the plot keeps free
    on its right when it has no such axis, so the plot keeps its place.
    soft_min keeps that value on the axis. The tooltip and the legend list
    the fields in the frame's order (after the transformations); a line
    drawn later is drawn over the earlier ones."""
    custom = {"drawStyle": "line", "lineInterpolation": "linear", "lineWidth": 1, "fillOpacity": 0, "showPoints": "never", "spanNulls": False,
              "axisPlacement": "auto", "axisBorderShow": False, "axisWidth": AXIS_PX, "scaleDistribution": {"type": "linear"},
              "hideFrom": {"legend": False, "tooltip": False, "viz": False}}
    if soft_min is not None:
        custom["axisSoftMin"] = soft_min
    thresholds = None
    if zero_line:
        custom["thresholdsStyle"] = {"mode": "dashed"}
        thresholds = {"mode": "absolute", "steps": [{"color": "transparent", "value": None}, {"color": GREY, "value": 0}]}
    opts = {"legend": {"displayMode": "table", "placement": "right", "width": LEGEND_PX - (AXIS_PX - 8 if right_axis else 0), "showLegend": True,
                       "calcs": list(calcs)},
            "tooltip": {"mode": "multi", "sort": "none", "hideZeros": False}}
    return panel("timeseries", title, q, x, y, w, h, description=description, unit="none", decimals=decimals, options=opts,
                 overrides=[legend_dash(line) for line in lines], custom=custom, thresholds=thresholds, no_value="No data for this selection",
                 source=source, transformations=transformations)


def legend_dash(line):
    """A line in the legend shows – where it has no value in the range: the panel's No data text in its row would widen the
    legend's columns and cut the series names."""
    props = {p["id"]: p["value"] for p in line["properties"]}
    if props.get("custom.hideFrom", {}).get("legend") or "noValue" in props:
        return line
    return dict(line, properties=line["properties"] + [{"id": "noValue", "value": "–"}])


def query_var(name, label, q, description, hide=0):
    return {"name": name, "label": label, "description": description, "type": "query", "datasource": dict(_ds), "definition": q, "query": q, "refresh": 1,
            "sort": 0, "multi": False, "includeAll": False, "current": {}, "options": [], "hide": hide, "allowCustomValue": False}


def custom_var(name, label, values, default, description, allow_custom=False):
    """values is a list of (text, value); a comma in a text is escaped in the query. allow_custom lets the viewer type a
    value that is not in the list; the default may then be one that is not."""
    opts = [{"text": t, "value": v, "selected": v == default} for t, v in values]
    text = next((t for t, v in values if v == default), default)
    assert allow_custom or text != default or any(v == default for _, v in values), default
    return {"name": name, "label": label, "description": description, "type": "custom",
            "query": ",".join("%s : %s" % (t.replace(",", "\\,"), v) for t, v in values),
            "options": opts, "current": {"text": text, "value": default, "selected": True}, "multi": False, "includeAll": False, "hide": 0,
            "allowCustomValue": allow_custom}


def textbox_var(name, label, default, description):
    """A free-text setting: the viewer types a number, in the unit the label names."""
    return {"name": name, "label": label, "description": description, "type": "textbox", "query": default,
            "current": {"text": default, "value": default}, "options": [{"selected": True, "text": default, "value": default}], "hide": 0}


def slug(text):
    return re.sub(r"[^a-z0-9]+", "-", text.lower()).strip("-")


def board_uid(name):
    return UID_PREFIX + "-" + name


def board_slug(name):
    return slug(BOARDS[name])


def reset_link(name, variables, time_from, refresh, keep, tooltip):
    """The Reset button on the controls bar: the board with its settings,
    the time range and the refresh spelled out at their defaults, since
    Grafana keeps the current values when a link opens the same board
    without parameters. The variables named in keep carry their current
    value."""
    from urllib.parse import quote
    params = ["orgId=1", "from=" + time_from, "to=now", "refresh=" + refresh]
    for v in variables:
        if v["name"] in keep:
            params.append("var-%s=${%s}" % (v["name"], v["name"]))
        elif v["hide"] != 2:
            params.append("var-%s=%s" % (v["name"], quote(v["current"]["value"], safe="")))
    return {"title": "Reset", "type": "link", "icon": "bolt", "url": "/d/%s/%s?%s" % (board_uid(name), board_slug(name), "&".join(params)),
            "tooltip": tooltip, "targetBlank": False, "keepTime": False, "includeVars": False}


def bands(name, q, color, panel_ids=None):
    """A shaded span for each (time, time_end, text) row of a query, on every
    chart or the given panels only. Always on: the bar holds only the settings."""
    mappings = {field: {"source": "field", "value": column} for field, column in (("time", "time"), ("timeEnd", "time_end"), ("text", "text"))}
    out = {"name": name, "datasource": dict(_ds), "enable": True, "hide": True, "iconColor": color, "rawQuery": q, "format": "table", "mappings": mappings}
    if panel_ids is not None:
        out["filter"] = {"exclude": False, "ids": panel_ids}
    return out


def marks(name, q, color, panel_ids):
    """A marker at the time of each (time, text) row of a query, on the
    given panels only. Always on, as the bands are."""
    mappings = {field: {"source": "field", "value": column} for field, column in (("time", "time"), ("text", "text"))}
    return {"name": name, "datasource": dict(_ds), "enable": True, "hide": True, "iconColor": color, "rawQuery": q, "format": "table", "mappings": mappings,
            "filter": {"exclude": False, "ids": panel_ids}}


def board_path(name):
    """The address given to users: /dashboards/<folder path>/<title>."""
    return "/dashboards/" + "/".join(slug(part) for part in FOLDER.split("/")) + "/" + board_slug(name)


def board_file(name):
    return os.path.join(STAGING, FOLDER, name + ".json")


def board_paths_conf():
    """The nginx rules that send a board's address to Grafana's own
    /d/<uid>/<slug>. try_files defers the redirect until after the login."""
    lines = ["# Generated by dashboard/grafana/gen_dashboard.py; edit the generator, not this file."]
    for name in BOARDS:
        path, at = board_path(name), "@" + board_uid(name)
        lines += ["location = %s { try_files /nonexistent %s; }" % (path, at),
                  "location = %s/ { try_files /nonexistent %s; }" % (path, at),
                  "location %s { return 302 /d/%s/%s$is_args$args; }" % (at, board_uid(name), board_slug(name))]
    return "\n".join(lines) + "\n"


def board_json(name, board):
    """The text of one board's file: the board with its uid, title and board-wide settings."""
    board = dict(board, uid=board_uid(name), title=BOARDS[name], timezone=TIMEZONE, editable=False, schemaVersion=SCHEMA_VERSION, graphTooltip=1)
    return json.dumps(board, indent=1, ensure_ascii=False) + "\n"


def write(name, board):
    """Writes one board to the staging directory. The preview Grafana reads
    it directly; the shared Grafana only sees it after deploy/install.sh has
    copied it."""
    path = board_file(name)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        f.write(board_json(name, board))
    return path
