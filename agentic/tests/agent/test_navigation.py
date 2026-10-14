"""open_in_app (contract I-1): validation as the user, the `navigate` SSE event, history and walk-mode context."""

from __future__ import annotations

import asyncio
from collections.abc import Iterator
from datetime import date
from types import SimpleNamespace
from typing import Any

import pytest
from google.adk.events import Event
from google.genai import types

from afdd_ai.agent import backend_api, navigation, prompts, stream
from afdd_ai.agent.agents import INSIGHT, ROOT, TWIN

from .support import ASSETS, TOKEN, Call, FakeBackend, FakeError, request_text
from .test_agent_flow import IDENTITY, first, function_response, make_service, names, run_turn
from .test_app_sessions import HEADERS, chat, client, scratch_db  # noqa: F401 - fixtures used by the HTTP test

PERMS = [
    "aiassistant.read", "digitaltwin.read", "aiinsights.read", "simulation.read", "equipmentoverview.read", "alarmsreport.read",
    "production.read", "maintenanceorder.read", "locationoverview.read", "earlywarningdiagnostics.read",
]  # fmt: skip
# A user whose location scope is FAB1 only (like the demo's fab facilities groups).
FAB1_TOKEN = "tok-fab1-nav"
FAB1_LOCATIONS = navigation.LOCATIONS[: navigation.LOCATIONS.index("MAU Plant Room South") + 1]


@pytest.fixture(autouse=True)
def locations_api(fake_backend: FakeBackend) -> Iterator[None]:
    """`filter/getalllocationsandassets` as the backend answers it: the user's visible locations."""
    assert FAB1_LOCATIONS[0] == "FAB1 Wafer Fab" and FAB1_LOCATIONS[-1] == "MAU Plant Room South"

    def locations(body: dict[str, Any], tok: str) -> list[dict[str, Any]]:
        visible = FAB1_LOCATIONS if tok == FAB1_TOKEN else navigation.LOCATIONS
        return [{"id": i + 1, "parent_id": None, "name": name, "assets": []} for i, name in enumerate(visible)]

    fake_backend.handlers["filter/getalllocationsandassets"] = locations
    fake_backend.add_user(FAB1_TOKEN, name="Nurul Aisyah")
    yield
    fake_backend.handlers.pop("filter/getalllocationsandassets", None)
    fake_backend.users.pop(FAB1_TOKEN, None)


@pytest.fixture
def today(monkeypatch: pytest.MonkeyPatch) -> date:
    """Plant today fixed to Thursday 2026-10-15."""
    monkeypatch.setattr(navigation, "_today", lambda: date(2026, 10, 15))
    return date(2026, 10, 15)


def ctx(permissions: list[str] | None = None, invocation_id: str | None = None, token: str = TOKEN, **ui: Any) -> SimpleNamespace:
    """A tool context; `ui` sets the UI temp state: walk mode (`ctx(walk=True)`) or a replay (`ctx(replay=85)`)."""
    state = {"temp:afdd_session": token, "temp:permissions": PERMS if permissions is None else permissions, "temp:user_id": 990001}
    if ui.get("walk"):
        state[navigation.UI_VIEW] = "walk"
    if ui.get("replay"):
        state[navigation.UI_REPLAY] = ui["replay"]
    return SimpleNamespace(state=state, invocation_id=invocation_id)


async def open_(context: SimpleNamespace | None = None, **kw: Any) -> dict[str, Any]:
    return await navigation.open_in_app(tool_context=context or ctx(), **kw)


def target(**kw: Any) -> dict[str, Any]:
    return {**dict.fromkeys(navigation.NAV_KEYS), **kw}


async def test_twin_target_resolves_equipment_and_parameter_as_the_user(fake_backend):
    out = await open_(page="digital-twin", asset="ch-2", parameter="CHW Leaving Temp", view="Walk", tab="faults", item_id=4)
    assert out["status"] == "opened"
    assert out["navigation"] == target(
        page="digital-twin", label="Walk to CH-02 CHW Leaving Temp (first person)", asset="CH_02", asset_id=2, parameter="chwst", view="walk"
    )
    assert "now shows: Walk to CH-02" in out["note"] and "Ignored (not used by digital-twin): tab, item_id." in out["note"]
    (params_call,) = fake_backend.calls_to("digital_twin/getassetparameters")
    assert params_call["body"] == {"asset_id": 2} and params_call["token"] == TOKEN and params_call["origin"] == "agent"
    assert fake_backend.calls_to("digital_twin/getassets")[0]["token"] == TOKEN

    tower = await open_(page="digital-twin", location=" cooling tower YARD ")
    assert tower["navigation"] == target(page="digital-twin", label="Cooling Tower Yard in the digital twin", location="Cooling Tower Yard")
    assert (await open_(page="digital-twin", location="tower yard", view="first person"))["navigation"]["view"] == "walk"
    both = await open_(page="digital-twin", asset="CT_01", location="Chiller Plant Room", panel="BMS overview")
    assert both["navigation"] == target(page="digital-twin", label="CT-01 with the BMS plant overview", asset="CT_01", asset_id=4, panel="plant-overview")
    assert "Ignored (not used by digital-twin): location." in both["note"]
    orbit = await open_(page="digital-twin", view="orbit", label="  Back to\n<orbit> view  ")
    assert orbit["navigation"]["label"] == "Back to orbit view" and orbit["navigation"]["view"] == "orbit"
    assert len((await open_(page="digital-twin", label="x" * 200))["navigation"]["label"]) == navigation.LABEL_MAX
    # the user's visible locations are read once (cached per user like the equipment list)
    assert len(fake_backend.calls_to("filter/getalllocationsandassets")) == 1


async def test_invalid_arguments_come_back_as_errors(fake_backend):
    cases = [
        ({"page": "dashboard"}, "Unknown page 'dashboard'"),
        ({"page": "digital-twin", "asset": "ZZ_99"}, "No visible equipment matches 'ZZ_99'"),
        ({"page": "digital-twin", "asset": "CT_01", "parameter": "fan_speed"}, "CT_01 has no parameter 'fan_speed'"),
        ({"page": "digital-twin", "parameter": "chwst"}, "parameter needs asset"),
        ({"page": "digital-twin", "location": "Basement"}, "Unknown location 'Basement'. Locations: FAB1 Wafer Fab"),
        ({"page": "digital-twin", "location": "room"}, "matches several locations"),
        ({"page": "digital-twin", "view": "fly"}, "Unknown view 'fly'. Use one of: walk, orbit."),
        ({"page": "digital-twin", "panel": "alarms"}, "Unknown panel 'alarms'"),
        ({"page": "ai-insights", "tab": "trends"}, "Unknown tab 'trends'"),
        ({"page": "ai-insights", "item_id": 5}, "item_id needs its tab"),
        ({"page": "ai-insights", "tab": "faults", "item_id": "abc"}, "item_id must be a whole number"),
        ({"page": "ai-insights", "tab": "faults", "item_id": -3}, "item_id must be a positive id"),
        ({"page": "simulation", "section": "results"}, "Unknown section 'results'. Use one of: new, new-plan."),
        ({"page": "asset-overview", "asset": "CH_01", "section": "history"}, "Unknown section 'history'"),
    ]
    for kwargs, expected in cases:
        out = await open_(**kwargs)
        assert set(out) == {"error"} and expected in out["error"], (kwargs, out)
    denied = await open_(ctx(["aiassistant.read"]), page="digital-twin", asset="CT_01")
    assert "needs the permission digitaltwin.read" in denied["error"]
    assert "navigation" in await open_(ctx(["admin"]), page="personnel-management")
    assert "No user session" in (await navigation.open_in_app(tool_context=SimpleNamespace(state={}), page="alarm-report"))["error"]


async def test_other_pages_keep_only_their_fields(fake_backend):
    fault = await open_(page="ai-insights", tab="Fault", item_id=43, asset="CT_01")
    assert fault["navigation"] == target(page="ai-insights", label="AI Insights: fault #43", tab="faults", item_id=43)
    assert "Ignored (not used by ai-insights): asset." in fault["note"]
    docs = await open_(page="ai-insights", tab="documents", item_id=5)
    assert docs["navigation"] == target(page="ai-insights", label="AI Insights: Documents", tab="documents")
    assert "item_id" in docs["note"]
    # fault injection is for administrators only (the page hides the tab from everyone else)
    assert "administrators only" in (await open_(page="ai-insights", tab="fault injection"))["error"]
    injection = await open_(ctx(["admin"]), page="ai-insights", tab="fault injection")
    assert injection["navigation"] == target(page="ai-insights", label="AI Insights: Injection", tab="injection")

    assert (await open_(page="simulation", item_id=85.0))["navigation"] == target(page="simulation", label="Simulation #85", item_id=85)
    assert (await open_(page="simulation", section="New"))["navigation"] == target(page="simulation", label="New simulation scenario", section="new")
    job = await open_(page="simulation", item_id=85, section="new")
    assert job["navigation"]["section"] is None and "section" in job["note"]

    # the top-level parameter narrows the detail page's parameter list (the page searches names)
    unit = await open_(page="asset-overview", asset="ct-1", section="SPC", parameter="chwst")
    assert unit["navigation"] == target(
        page="asset-overview", label="CT-01 equipment details (SPC) · search 'CHW Leaving Temp'", asset="CT_01", asset_id=4, section="spc",
        filters={"parameter-search": ["CHW Leaving Temp"]},
    )  # fmt: skip
    assert "Ignored" not in unit["note"] and len(fake_backend.calls_to("digital_twin/getassetparameters")) == 1
    listing = await open_(page="asset-overview", section="status")
    assert listing["navigation"] == target(page="asset-overview", label="Equipment overview") and "section" in listing["note"]
    report = await open_(page="alarm-report", asset="CH_01", view="walk")
    assert report["navigation"] == target(page="alarm-report", label="Alarm report")
    assert "Ignored (not used by alarm-report): asset, view." in report["note"]


async def test_fab1_units_and_places_are_modelled(fake_backend):
    extra = [
        {**ASSETS[0], "id": 31, "alias": "MAU_01", "name": "Make-up Air Unit MAU-01", "type_name": "Make-up Air Unit"},
        {**ASSETS[0], "id": 33, "alias": "DP_04", "name": "Dry Pump DP-04", "type_name": "Dry Vacuum Pump"},
    ]
    fake_backend.handlers["digital_twin/getassets"] = lambda body, tok: ASSETS + extra
    try:
        mau = await open_(page="digital-twin", asset="MAU_01", view="walk")
        assert mau["navigation"]["asset_id"] == 31 and mau["navigation"]["view"] == "walk" and "3D" not in mau["note"]
        dp = await open_(page="digital-twin", asset="dp-4")
        assert dp["navigation"]["asset"] == "DP_04" and "3D" not in dp["note"]
        room = await open_(page="digital-twin", location="MAU Plant Room North", view="walk")
        assert room["navigation"]["location"] == "MAU Plant Room North" and room["navigation"]["view"] == "walk"
        fab = await open_(page="digital-twin", location="fab1 wafer fab")
        assert fab["navigation"]["location"] == "FAB1 Wafer Fab" and "3D" not in fab["note"]
    finally:
        fake_backend._install_defaults()


async def test_units_and_places_without_3d_models_are_flagged(fake_backend, monkeypatch):
    # every unit and location is modelled today: take the MAUs and a bay out to keep the path tested
    monkeypatch.setattr(navigation, "MODELLED_KINDS", navigation.MODELLED_KINDS - {"MAU"})
    monkeypatch.setattr(navigation, "LOCATIONS_3D", navigation.LOCATIONS_3D - {"Sub-Fab North", "MAU Plant Room North"})
    no_room = await open_(page="digital-twin", location="MAU Plant Room North", view="walk")
    assert "MAU Plant Room North has no 3D model, so walk mode cannot go there" in no_room["error"]
    extra = [
        {**ASSETS[0], "id": 31, "alias": "MAU_01", "name": "Make-up Air Unit MAU-01", "type_name": "Make-up Air Unit"},
        {**ASSETS[0], "id": 32, "alias": "PCWP_02", "name": "PCW Pump PCWP-02", "type_name": "Process Cooling Water Pump"},
    ]
    fake_backend.handlers["digital_twin/getassets"] = lambda body, tok: ASSETS + extra
    try:
        mau = await open_(page="digital-twin", asset="MAU_01")
        assert mau["navigation"]["label"] == "MAU-01 in the digital twin" and "MAU_01 is not modelled in 3D yet" in mau["note"]
        walk = await open_(page="digital-twin", asset="MAU_01", view="walk")  # nowhere to stand: refused like a location
        assert "MAU_01 has no 3D model (nor has its location), so walk mode cannot go there" in walk["error"]
        pcw = await open_(page="digital-twin", asset="pcwp-2", view="walk")  # modelled since the PCW room was built
        assert pcw["navigation"]["asset_id"] == 32 and pcw["navigation"]["view"] == "walk"
        assert "3D" not in pcw["note"]
        walking = await open_(ctx(walk=True), page="digital-twin", asset="MAU_01")
        assert walking["navigation"]["view"] is None and "the twin leaves it" in walking["note"]
        ct = await open_(page="digital-twin", asset="CT_01")
        assert "3D" not in ct["note"]
        fab = await open_(page="digital-twin", location="sub-fab bay a (etch)")
        assert fab["navigation"]["location"] == "Sub-Fab North" and "has no 3D model yet" in fab["note"]
        empty = await open_(ctx(walk=True), page="digital-twin", location="Sub-Fab North")
        assert empty["navigation"]["view"] is None and "nothing in 3D to walk to" in empty["note"]
    finally:
        fake_backend._install_defaults()


async def test_unmodelled_unit_in_a_3d_room_is_shown_in_its_room(fake_backend, monkeypatch):
    # no such unit today (every CUB unit is modelled): the path stays for rooms built before their equipment
    monkeypatch.setattr(navigation, "MODELLED_KINDS", navigation.MODELLED_KINDS - {"PCWP"})
    monkeypatch.setattr(navigation, "ROOM_ONLY_KINDS", {"PCWP": "PCW Pump Room"})
    extra = [{**ASSETS[0], "id": 32, "alias": "PCWP_02", "name": "PCW Pump PCWP-02", "type_name": "Process Cooling Water Pump"}]
    fake_backend.handlers["digital_twin/getassets"] = lambda body, tok: ASSETS + extra
    try:
        pcw = await open_(page="digital-twin", asset="pcwp-2", view="walk")
        assert pcw["navigation"]["asset_id"] == 32 and pcw["navigation"]["view"] == "walk"
        assert "the twin shows its room (PCW Pump Room)" in pcw["note"]
    finally:
        fake_backend._install_defaults()


async def test_one_navigation_per_answer(fake_backend):
    turn = ctx(invocation_id="inv-1")
    assert "error" in await open_(turn, page="digital-twin", asset="ZZ_99")  # a failed call does not count
    assert (await open_(turn, page="digital-twin", asset="CT_01"))["status"] == "opened"
    again = await open_(turn, page="alarm-report")
    assert "already opened (or is opening) a page" in again["error"]
    turn.invocation_id = "inv-2"  # the next answer may navigate again
    assert (await open_(turn, page="alarm-report"))["status"] == "opened"


async def test_of_concurrent_calls_in_one_answer_the_first_wins(fake_backend):
    turn = ctx(invocation_id="inv-p")
    # the first call claims the answer's navigation before its backend lookups, so a later call that
    # needs none (or finds its data cached) cannot overtake it
    unit, report = await asyncio.gather(open_(turn, page="digital-twin", asset="CT_01"), open_(turn, page="alarm-report"))
    assert unit["navigation"]["asset"] == "CT_01" and "already opened (or is opening) a page" in report["error"]
    turn.invocation_id = "inv-q"
    backend_api.clear_cache()  # the first call waits for the equipment list again
    bad, other = await asyncio.gather(open_(turn, page="digital-twin", asset="ZZ_99"), open_(turn, page="alarm-report"))
    assert "No visible equipment" in bad["error"] and "already opened (or is opening)" in other["error"]
    assert turn.state[navigation.NAV_STATE] == "inv-p"  # the failed first call gave its claim back
    assert (await open_(turn, page="alarm-report"))["status"] == "opened"


async def test_a_walking_user_stays_in_walk_mode(fake_backend):
    unit = await open_(ctx(walk=True), page="digital-twin", asset="CT_01")
    assert unit["navigation"] == target(page="digital-twin", label="Walk to CT-01 (first person)", asset="CT_01", asset_id=4, view="walk")
    assert (await open_(ctx(walk=True), page="digital-twin", panel="plant-overview"))["navigation"]["view"] == "walk"
    assert (await open_(ctx(walk=True), page="digital-twin", location="tower yard"))["navigation"]["view"] == "walk"
    assert (await open_(ctx(walk=True), page="digital-twin", view="orbit"))["navigation"]["view"] == "orbit"  # only orbit leaves
    bay = await open_(ctx(walk=True), page="digital-twin", location="Sub-Fab North")
    assert bay["navigation"]["view"] == "walk"  # every FAB1 location has a 3D scene since 2026-10-12
    assert (await open_(ctx(walk=True), page="alarm-report"))["navigation"]["view"] is None
    assert (await open_(page="digital-twin", asset="CT_01"))["navigation"]["view"] is None  # not walking


async def test_a_twin_target_during_a_replay_stays_in_the_replay(fake_backend):
    out = await open_(ctx(replay=85), page="digital-twin", asset="CT_01", label="x" * 200)
    suffix = navigation.REPLAY_SUFFIX.format(job=85)
    assert out["navigation"]["label"].endswith("…" + suffix) and len(out["navigation"]["label"]) == navigation.LABEL_MAX
    assert "The twin stays in the replay of simulation #85: it shows the replayed run, not live data." in out["note"]
    yard = await open_(ctx(replay=85), page="digital-twin", location="tower yard")
    assert yard["navigation"]["label"] == "Cooling Tower Yard in the digital twin (replay of simulation #85)"
    report = await open_(ctx(replay=85), page="alarm-report")
    assert report["navigation"]["label"] == "Alarm report" and "replay" not in report["note"]


def test_ui_state_from_the_chat_context():
    view, replay = navigation.UI_VIEW, navigation.UI_REPLAY
    assert navigation.ui_state({"page": "digital-twin", "view": "walk", "replay_job_id": "85"}) == {view: "walk", replay: 85}
    # the simulation page sends its selected job as replay_job_id too: that is no twin replay
    assert navigation.ui_state({"page": "simulation", "view": "walk", "replay_job_id": 85}) == {view: None, replay: None}
    assert navigation.ui_state({"page": "digital-twin", "view": "orbit", "replay_job_id": "x"}) == {view: None, replay: None}
    assert navigation.ui_state(None) == {view: None, replay: None}


async def test_places_outside_the_users_location_scope_are_refused(fake_backend):
    fab = ctx(token=FAB1_TOKEN)
    yard = await open_(fab, page="digital-twin", location="cooling tower yard")
    assert "Cooling Tower Yard is outside the user's location access" in yard["error"]
    overview = await open_(fab, page="digital-twin", panel="plant-overview")
    assert "BMS plant overview shows the chiller plant (Chiller Plant Room), which is outside the user's location access" in overview["error"]
    bay = await open_(fab, page="digital-twin", location="bay a")
    assert bay["navigation"]["location"] == "Sub-Fab North"
    unknown = await open_(fab, page="digital-twin", location="Basement")
    assert "Unknown location 'Basement'. Locations: FAB1 Wafer Fab, FAB1 L1 Sub-Fab," in unknown["error"] and "Cooling" not in unknown["error"]
    assert all(c["token"] == FAB1_TOKEN for c in fake_backend.calls_to("filter/getalllocationsandassets"))


async def test_the_models_label_is_used_only_when_every_field_was_used(fake_backend):
    report = await open_(page="alarm-report", asset="CH_01", label="CH-01 alarms")
    assert report["navigation"]["label"] == "Alarm report" and "Ignored (not used by alarm-report): asset." in report["note"]
    twin = await open_(page="digital-twin", asset="CT_01", location="Chiller Plant Room", label="The chiller plant room")
    assert twin["navigation"]["label"] == "CT-01 in the digital twin"
    job = await open_(page="simulation", item_id=85, section="new", label="New scenario")
    assert job["navigation"]["label"] == "Simulation #85"
    docs = await open_(page="ai-insights", tab="documents", item_id=5, label="Document #5")
    assert docs["navigation"]["label"] == "AI Insights: Documents"
    assert (await open_(page="digital-twin", asset="CT_01", label="Cooling tower 1"))["navigation"]["label"] == "Cooling tower 1"


# URL keys each page reads (the assistant UI appends exactly these; contract B.10).
PAGE_URL_KEYS = {
    "production": ["tab", "window", "lot-status", "lot-search", "lot", "char", "quality-window", "quality-tool", "timeline-window"],
    "plant-overview": ["location", "period"],
    "ai-insights": ["status", "asset", "rule", "hours", "display", "detector", "q", "type", "unit"],
    "simulation": ["status", "origin", "domain"],
    "asset-overview": ["recent", "date", "parameter-search"],
    "maintenance-order": ["view", "maintenanceStatusIds", "severityIds", "priorityIds", "maintenanceTypeIds", "assetIds", "no-locationIds-filter", "start", "end", "search"],
    "alarm-report": ["eventview", "alarmTypeIds", "assetIds", "no-locationIds-filter", "start", "end", "search"],
    "early-warning-and-diagnostics": ["severityIds", "assetIds", "no-locationIds-filter", "start", "end", "search"],
}


async def test_maintenance_order_filters_become_its_url_params(fake_backend, today):
    out = await open_(
        page="maintenance-order",
        filters={"status": ["open", "In Progress"], "Severity": "critical", "assets": ["ch-1", "CT_01"], "start": "2026-10-08", "end": "2026-10-15", "search": " pump "},
    )
    nav = out["navigation"]
    assert nav["filters"] == {
        "maintenanceStatusIds": ["3", "4"], "severityIds": ["1"], "assetIds": ["1", "4"], "no-locationIds-filter": ["true"],
        "start": ["2026-10-08"], "end": ["2026-10-15"], "search": ["pump"],
    }  # fmt: skip
    # 103 characters of filter text: cut at LABEL_MAX
    assert len(nav["label"]) == navigation.LABEL_MAX and nav["label"].endswith("…")
    assert nav["label"].startswith("Maintenance orders · open, in progress, critical, CH-01, CT-01, 2026-10-08 to")
    assert "Ignored" not in out["note"] and "today's orders" not in out["note"]
    assert fake_backend.calls_to("digital_twin/getassets")[0]["token"] == TOKEN

    week = await open_(page="maintenance-order", filters={"status": ["outstanding"], "severity": ["critical"], "equipment": ["CH_01"], "start": "this-week"})
    assert week["navigation"]["filters"] == {
        "maintenanceStatusIds": ["1", "3", "4"], "severityIds": ["1"], "assetIds": ["1"], "no-locationIds-filter": ["true"],
        "start": ["2026-10-12"], "end": ["2026-10-18"],  # the order list needs the rest of the week too
    }  # fmt: skip
    assert week["navigation"]["label"] == "Maintenance orders · outstanding, critical, CH-01, this week"

    async def status_ids(status: Any) -> list[str] | None:
        return (await open_(page="maintenance-order", filters={"status": status, "start": "today"}))["navigation"]["filters"].get("maintenanceStatusIds")

    assert await status_ids("pending") == ["1", "3", "4"]  # an alias of outstanding
    assert await status_ids("paused") == ["7"]
    assert await status_ids("open, done") == ["3", "5"]  # a comma-separated list
    assert await status_ids(["requested", "rejected", "open", "in-progress", "paused", "done", "closed"]) is None  # all: the page's "All"
    merged = await open_(page="maintenance-order", filters={"status": "open", "statuses": ["done"], "severity": ["critical", "high", "medium", "low"], "start": "today"})
    assert merged["navigation"]["filters"] == {"maintenanceStatusIds": ["3", "5"], "start": ["2026-10-15"], "end": ["2026-10-15"]}
    period = await open_(page="maintenance-order", section="calendar", filters={"period": "this week", "type": "PM", "priority": "high"})
    assert period["navigation"]["filters"] == {"view": ["calendar"], "priorityIds": ["1"], "maintenanceTypeIds": ["2"], "start": ["2026-10-12"], "end": ["2026-10-18"]}
    assert period["navigation"]["label"] == "Maintenance orders: calendar · high, preventive, this week"
    assert (await open_(page="maintenance-order", filters={"equipment": [4.0], "start": "today"}))["navigation"]["filters"]["assetIds"] == ["4"]
    today_only = await open_(page="maintenance-order", filters={"severity": "high"})
    assert "Without a period the page lists only today's orders; say so." in today_only["note"]
    assert (await open_(page="maintenance-order", section="table"))["navigation"]["filters"] == {"view": ["list"]}


@pytest.mark.parametrize(
    ("given", "start", "end"),
    [
        ({"start": "today"}, "2026-10-15", "2026-10-15"),
        ({"start": "yesterday"}, "2026-10-14", "2026-10-14"),
        ({"start": "this-week"}, "2026-10-12", "2026-10-15"),
        ({"start": "last 7 days"}, "2026-10-09", "2026-10-15"),
        ({"start": "last-30-days"}, "2026-09-16", "2026-10-15"),
        ({"end": "this-week"}, "2026-10-12", "2026-10-15"),
        ({"start": "2026-10-01"}, "2026-10-01", "2026-10-15"),
        ({"start": "2026-10-20"}, "2026-10-20", "2026-10-20"),
        ({"end": "2026-10-03"}, "2026-10-03", "2026-10-03"),
        ({"date": "yesterday"}, "2026-10-14", "2026-10-14"),
        ({"day": "2026-10-01"}, "2026-10-01", "2026-10-01"),  # an ISO date under date / day / period / window is that day
        ({"date": "2026-10-01", "end": "2026-10-03"}, "2026-10-01", "2026-10-03"),
        ({"from": "today", "to": "today"}, "2026-10-15", "2026-10-15"),
    ],
)
async def test_relative_and_open_ended_periods(fake_backend, today, given, start, end):
    out = await open_(page="alarm-report", filters=given)
    assert out["navigation"]["filters"] == {"start": [start], "end": [end]}


async def test_invalid_filters_come_back_as_errors(fake_backend, today):
    cases = [
        ({"page": "maintenance-order", "filters": {"status": "stuck"}}, "Unknown status filter 'stuck'. Use one of: requested, rejected, open,"),
        ({"page": "maintenance-order", "filters": {"severity": ["high", "urgent"]}}, "Unknown severity filter 'urgent'"),
        ({"page": "maintenance-order", "filters": {"severity": True}}, "severity must be a value or a list of values."),
        ({"page": "maintenance-order", "filters": {"severity": [{"x": 1}]}}, "severity must be a value or a list of values."),
        ({"page": "alarm-report", "filters": {"start": "10/08/2026"}}, "start must be a date like 2026-10-08 or one of today, yesterday, this-week"),
        ({"page": "alarm-report", "filters": {"start": "2026-10-10", "end": "2026-10-01"}}, "start (2026-10-10) is after end (2026-10-01)."),
        ({"page": "alarm-report", "filters": {"start": "2025-09-01", "end": "2026-10-05"}}, "The period is longer than 366 days"),
        ({"page": "alarm-report", "filters": {"start": "today", "end": "2026-10-01"}}, "Use a relative period (e.g. last-7-days) alone, without another date."),
        ({"page": "alarm-report", "filters": {"start": ["2026-10-01", "2026-10-02"]}}, "start takes one value (got 2026-10-01, 2026-10-02)."),
        ({"page": "maintenance-order", "filters": "status=open"}, 'filters must be an object of filter names and values, e.g. {"status": ["open"]}.'),
        ({"page": "maintenance-order", "filters": []}, "filters must be an object of filter names and values"),
        ({"page": "ai-insights", "filters": {"status": "cleared"}}, "AI Insights filters need their tab"),
        ({"page": "ai-insights", "tab": "faults", "filters": {"equipment": ["CH_01", "CT_01"]}}, "equipment takes one value (got CH_01, CT_01)."),
        ({"page": "ai-insights", "tab": "faults", "filters": {"rule": "11"}}, "rule must be a fault rule code like CHL-011 (got '11')."),
        ({"page": "production", "filters": {"window": "fortnight"}}, "Unknown window filter 'fortnight'. Use one of: 12, 24, 168, 720."),
        ({"page": "production", "filters": {"lot": "M26 01"}}, "lot must be a code like the ones the production tools return (got 'M26 01')."),
        ({"page": "asset-overview", "asset": "CT_01", "filters": {"date": "last-7-days"}}, "date is one day: use a date like 2026-10-08, today or yesterday"),
        ({"page": "asset-overview", "asset": "CT_01", "filters": {"parameter": "fan_speed"}}, "CT_01 has no parameter 'fan_speed'"),
        ({"page": "asset-overview", "asset": "CT_01", "filters": {"parameter": "chwst", "search": "fan"}}, "parameter_search takes one value (got chwst, fan)."),
        ({"page": "maintenance-order", "filters": {"equipment": "ZZ_99"}}, "No visible equipment matches 'ZZ_99'"),
        ({"page": "plant-overview", "filters": {"location": "Basement"}}, "Unknown location 'Basement'"),
    ]
    for kwargs, expected in cases:
        out = await open_(**kwargs)
        assert set(out) == {"error"} and expected in out["error"], (kwargs, out)
    fake_backend.handlers["digital_twin/getassets"] = lambda body, tok: [] if tok == FAB1_TOKEN else ASSETS
    try:
        hidden = await open_(ctx(token=FAB1_TOKEN), page="maintenance-order", filters={"equipment": ["CH_01"]})
        assert "No visible equipment matches 'CH_01'" in hidden["error"]
    finally:
        fake_backend._install_defaults()
    room = await open_(ctx(token=FAB1_TOKEN), page="plant-overview", filters={"location": "Chiller Plant Room"})
    assert "Chiller Plant Room is outside the user's location access" in room["error"]


async def test_users_without_twin_access_resolve_equipment_by_name(fake_backend, today):
    """Without digitaltwin.read the equipment comes from the filter tree: names, no aliases."""

    def denied(body: dict[str, Any], tok: str) -> Any:
        raise FakeError(403, "Permission denied")

    tree = fake_backend.handlers["filter/getalllocationsandassets"]
    fake_backend.handlers["digital_twin/getassets"] = denied
    seed = {1: "Chiller CH-01", 5: "CW Pump CWP-01", 41: "Process Chiller PCH-01", 42: "PCW Pump PCWP-01"}  # the seed's names
    units = [{"id": i, "name": name} for i, name in seed.items()]
    fake_backend.handlers["filter/getalllocationsandassets"] = lambda body, tok: [{"id": 16, "parent_id": None, "name": "Chiller Plant Room", "assets": units}]
    maintenance_only = ctx([p for p in PERMS if p != "digitaltwin.read"])
    try:
        orders = await open_(maintenance_only, page="maintenance-order", filters={"equipment": ["CH_01", "cwp-1", "CH-01", "PCWP_01"], "start": "today"})
        assert orders["navigation"]["filters"]["assetIds"] == ["1", "5", "42"]  # by the tag in the name, not inside PCH-01 / PCWP-01
        docs = await open_(maintenance_only, page="ai-insights", tab="documents", filters={"equipment": "CH_01"})
        assert docs["navigation"]["filters"] is None and "Ignored (not used by ai-insights): filters.equipment." in docs["note"]
        unit = await open_(maintenance_only, page="asset-overview", asset="1", filters={"parameter": "fan_speed"})
        assert unit["navigation"]["filters"] == {"parameter-search": ["fan speed"]}  # its parameters cannot be read: as text
        assert not fake_backend.calls_to("digital_twin/getassetparameters")
    finally:
        fake_backend._install_defaults()
        fake_backend.handlers["filter/getalllocationsandassets"] = tree


async def test_filters_that_do_not_apply_are_ignored(fake_backend, today):
    twin = await open_(page="digital-twin", asset="CT_01", filters={"status": "open"})
    assert twin["navigation"]["filters"] is None and "Ignored (not used by digital-twin): filters.status." in twin["note"]
    walk = await open_(page="digital-twin", asset="CT_01", filters={"walk": True, "empty": ""})  # ignored filters are not validated
    assert walk["navigation"]["asset"] == "CT_01" and "Ignored (not used by digital-twin): filters.walk." in walk["note"]
    rules = await open_(page="ai-insights", tab="rules", filters={"status": {"x": 1}})
    assert rules["navigation"]["filters"] is None and "filters.status" in rules["note"]
    colour = await open_(page="maintenance-order", label="Red orders", filters={"colour": "red", "start": "today"})
    assert colour["navigation"]["label"] == "Maintenance orders · today" and "Ignored (not used by maintenance-order): filters.colour." in colour["note"]
    quality = await open_(page="production", section="quality", filters={"lot_status": "hold"})
    assert quality["navigation"]["filters"] is None and "filters.lot_status" in quality["note"]
    kpi = await open_(page="production", section="kpi", filters={"quality_window": "30d", "window": "24h"})
    assert kpi["navigation"]["filters"] == {"window": ["24"]} and "filters.quality_window" in kpi["note"]
    detector = await open_(page="ai-insights", tab="faults", filters={"detector": "zscore"})
    assert detector["navigation"]["filters"] is None and "filters.detector" in detector["note"]
    active = await open_(page="ai-insights", tab="faults", filters={"status": "active", "window": "7d"})
    assert active["navigation"]["filters"] == {"status": ["ACTIVE"]} and "filters.window" in active["note"]
    listing = await open_(page="asset-overview", filters={"date": "today"})
    assert listing["navigation"]["filters"] is None and "filters.date" in listing["note"]
    chiller = await open_(page="simulation", section="new-plan", filters={"workspace": "chiller"})
    assert chiller["navigation"]["filters"] is None and "filters.workspace" in chiller["note"]
    fab = await open_(page="simulation", section="new-plan", filters={"workspace": "fab"})
    assert fab["navigation"]["filters"] is None and "Ignored" not in fab["note"]


async def test_production_sections_and_filters(fake_backend):
    hold = (await open_(page="production", filters={"lot_status": "on hold"}))["navigation"]
    assert (hold["section"], hold["filters"], hold["label"]) == ("lots", {"lot-status": ["hold"]}, "Production: lots · hold")
    assert (await open_(page="production", section="lots"))["navigation"]["filters"] is None  # the UI sets a tab id's tab itself
    scrap = (await open_(page="production", section="scrap pareto"))["navigation"]
    assert (scrap["section"], scrap["filters"], scrap["label"]) == ("scrap", {"tab": ["quality"]}, "Production: scrap")
    kpi = (await open_(page="production", section="kpi"))["navigation"]
    assert (kpi["section"], kpi["filters"]) == ("kpi", None)
    assert (await open_(page="production", section="oee"))["navigation"]["section"] == "oee"  # the KPI card, no longer the overview tab
    assert (await open_(page="production", filters={"window": "week"}))["navigation"]["filters"] == {"window": ["168"]}
    assert (await open_(page="production", filters={"window": 168.0}))["navigation"]["filters"] == {"window": ["168"]}
    assert (await open_(page="production", filters={"window": ["7d", "week"]}))["navigation"]["filters"] == {"window": ["168"]}  # one value
    tool = (await open_(page="production", filters={"quality_tool": "CT_01"}))["navigation"]
    assert (tool["section"], tool["filters"]) == ("quality", {"quality-tool": ["4"]})
    lot = await open_(page="production", filters={"lot": "m261013.01", "char": "padox.thk"})
    assert (lot["navigation"]["section"], lot["navigation"]["filters"]) == ("lots", {"lot": ["M261013.01"]})
    assert "Ignored (not used by production): filters.characteristic." in lot["note"]
    chart = (await open_(page="production", section="spc chart", filters={"char_key": "padox.thk", "quality_window": "month"}))["navigation"]
    assert chart["filters"] == {"tab": ["quality"], "char": ["PADOX.THK"], "quality-window": ["720"]}
    assert chart["label"] == "Production: characteristic · PADOX.THK, last 30 days"


async def test_ai_insights_filters_belong_to_the_tab(fake_backend):
    faults = (await open_(page="ai-insights", tab="faults", filters={"status": "cleared", "equipment": "CT_01", "window": "30d", "display": "timeline"}))["navigation"]
    assert faults["filters"] == {"status": ["CLEARED"], "asset": ["4"], "hours": ["720"], "display": ["timeline"]}
    assert faults["label"] == "AI Insights: Faults · cleared, CT-01, last 30 days, timeline"
    # the page ignores the window while it shows the active faults: a window alone shows all of them
    assert (await open_(page="ai-insights", tab="faults", filters={"window": "7d"}))["navigation"]["filters"] == {"status": ["all"], "hours": ["168"]}
    anomalies = await open_(page="ai-insights", tab="anomaly", filters={"detector": "z-score", "units": "CH_02", "window": "day"})
    assert anomalies["navigation"]["filters"] == {"status": ["all"], "detector": ["zscore"], "asset": ["2"], "hours": ["24"]}
    docs = await open_(page="ai-insights", tab="documents", filters={"type": "report", "equipment": "ch-1", "status": "indexed"})
    assert docs["navigation"]["filters"] == {"type": ["maintenance_report"], "status": ["READY"], "unit": ["CH_01"]}
    long = await open_(page="ai-insights", tab="documents", filters={"search": "x" * 100})
    assert long["navigation"]["filters"] == {"q": ["x" * 60]}  # cut without an ellipsis
    cleared = await open_(page="ai-insights", tab="faults", filters={"status": "Cleared", "statuses": "cleared"})  # one value, two spellings
    assert cleared["navigation"]["filters"] == {"status": ["CLEARED"]}
    recs = await open_(page="ai-insights", tab="recommendations", filters={"status": "validated"})
    assert recs["navigation"]["filters"] == {"status": ["VALIDATED"]}
    assert (await open_(page="ai-insights", tab="recommendations", filters={"status": "all"}))["navigation"]["filters"] is None


async def test_simulation_and_plant_overview_filters(fake_backend):
    assert (await open_(page="simulation", section="new-plan"))["navigation"]["filters"] is None  # the UI opens the fab workspace
    jobs = await open_(page="simulation", filters={"status": "needs approval", "origin": "assistant"})
    assert jobs["navigation"]["filters"] == {"status": ["approval"], "origin": ["agent"]}
    assert jobs["navigation"]["label"] == "Simulation jobs · approval, agent"
    assert (await open_(page="simulation", filters={"status": "all", "workspace": "wafer fab"}))["navigation"]["filters"] == {"domain": ["fab"]}

    plant = await open_(page="plant-overview", filters={"location": "chiller plant room", "period": "30d"})
    assert plant["navigation"]["filters"] == {"location": ["16"], "period": ["30"]}  # the fixture's ids are index + 1
    assert plant["navigation"]["label"] == "Plant overview · Chiller Plant Room, last 30 days"
    top = await open_(page="plant-overview", location="Chiller Plant Room")  # where the model predictably puts it
    assert top["navigation"]["filters"] == {"location": ["16"]} and top["navigation"]["location"] is None and "Ignored" not in top["note"]
    both = await open_(page="plant-overview", location="CUB Roof", section="tiles", filters={"location": "Chiller Plant Room", "period": "today"})
    assert both["navigation"]["filters"] == {"location": ["16"], "period": ["1"]} and "Ignored (not used by plant-overview): location." in both["note"]
    assert both["navigation"]["label"] == "Plant overview: summary · Chiller Plant Room, today"
    assert "needs the permission locationoverview.read" in (await open_(ctx(["aiassistant.read"]), page="plant-overview"))["error"]
    assert len(fake_backend.calls_to("filter/getalllocationsandassets")) == 1  # the location rows are cached per user

    # a leaf location has no cards of its own: the page shows its parent's
    rows = [{"id": i + 1, "parent_id": 15 if name == "Chiller Plant Room" else None, "name": name, "assets": []} for i, name in enumerate(navigation.LOCATIONS)]
    fake_backend.handlers["filter/getalllocationsandassets"] = lambda body, tok: rows
    backend_api.clear_cache()
    leaf = (await open_(page="plant-overview", filters={"location": "Chiller Plant Room"}))["navigation"]
    assert (leaf["filters"], leaf["section"], leaf["label"]) == ({"location": ["15"]}, "locations", "Plant overview: locations · Chiller Plant Room")


async def test_alarm_report_early_warning_and_asset_filters(fake_backend, today):
    events = (await open_(page="alarm-report", section="events", filters={"alarm_type": "abnormal", "equipment": "CT_01", "start": "yesterday"}))["navigation"]
    assert events["filters"] == {
        "eventview": ["true"], "alarmTypeIds": ["ABNORMAL"], "assetIds": ["4"], "no-locationIds-filter": ["true"], "start": ["2026-10-14"], "end": ["2026-10-14"],
    }  # fmt: skip
    assert events["label"] == "Alarm report: events · abnormal, CT-01, yesterday"
    for both in (["alarm", "abnormal"], "both", ["alarm", "both"]):  # the page shows one type: both is its "all"
        out = (await open_(page="alarm-report", section="report", filters={"alarm_type": both}))["navigation"]
        assert out["filters"] == {"eventview": ["false"]} and out["label"] == "Alarm report: report · alarm, abnormal"

    warnings = await open_(page="early-warning-and-diagnostics", filters={"severity": ["high"]})
    assert warnings["navigation"]["filters"] == {"severityIds": ["2"]}
    assert "Without a period the page lists only today's warnings; say so." in warnings["note"]
    assert (await open_(page="early-warning-and-diagnostics", section="diagnosis"))["navigation"]["section"] == "diagnostics"
    assert "Unknown section 'degradation'" in (await open_(page="early-warning-and-diagnostics", section="degradation"))["error"]

    alarms = (await open_(page="asset-overview", asset="CT_01", section="alarms", filters={"date": "yesterday", "parameter_search": "fan"}))["navigation"]
    assert alarms["filters"] == {"recent": ["alarm"], "date": ["2026-10-14"], "parameter-search": ["fan"]}
    assert alarms["label"] == "CT-01 equipment details (recent alarms) · yesterday, search 'fan'"
    power = await open_(page="asset-overview", asset="CT_01", filters={"parameter": "power_kw"})
    assert power["navigation"]["filters"] == {"parameter-search": ["Power"]}  # the page searches names, not aliases
    orders = (await open_(page="asset-overview", asset="CT_01", section="recent maintenance"))["navigation"]
    assert (orders["section"], orders["filters"]) == ("maintenance", {"recent": ["maintenance"]})
    named = await open_(page="asset-overview", asset="CT_01", parameter="chwst", filters={"search": "pow"})
    assert named["navigation"]["filters"] == {"parameter-search": ["pow"]} and "Ignored (not used by asset-overview): parameter." in named["note"]


async def test_every_emitted_url_key_is_one_the_page_reads(fake_backend, today):
    samples = [
        ("production", {"section": "scrap", "filters": {"window": "24h", "quality_window": "30d", "char": "DEFECTS", "tool": "CT_01"}}),
        ("production", {"filters": {"lot_status": "hold", "lot_search": "M26", "lot": "M261013.01"}}),
        ("production", {"section": "timeline", "filters": {"timeline_window": "3d"}}),
        ("plant-overview", {"filters": {"location": "CUB Roof", "period": "7d"}}),
        ("ai-insights", {"tab": "faults", "filters": {"status": "cleared", "equipment": "CT_01", "rule": "chl-011", "window": "90d", "display": "table"}}),
        ("ai-insights", {"tab": "anomalies", "filters": {"detector": "ewma", "window": "24h"}}),
        ("ai-insights", {"tab": "recommendations", "filters": {"status": "validated"}}),
        ("ai-insights", {"tab": "documents", "filters": {"q": "pump", "type": "sop", "status": "ready", "unit": "CH_01"}}),
        ("simulation", {"filters": {"status": "failed", "origin": "ui", "workspace": "fab"}}),
        ("asset-overview", {"asset": "CT_01", "section": "maintenance", "filters": {"date": "2026-10-14", "search": "power"}}),
        ("maintenance-order", {"section": "list", "filters": {"status": "open", "severity": "low", "priority": "high", "type": "pm", "equipment": "CT_01", "start": "last-7-days", "search": "pump"}}),
        ("alarm-report", {"section": "report", "filters": {"type": "alarm", "equipment": "CT_01", "start": "today", "search": "trip"}}),
        ("early-warning-and-diagnostics", {"filters": {"severity": "critical", "equipment": "CT_01", "from": "2026-10-01", "to": "2026-10-14", "search": "pump"}}),
    ]
    emitted: dict[str, set[str]] = {}
    for page, kwargs in samples:
        nav = (await open_(page=page, **kwargs))["navigation"]
        assert nav["filters"] and set(nav["filters"]) <= set(PAGE_URL_KEYS[page]), (page, nav["filters"])
        assert all(isinstance(v, str) for values in nav["filters"].values() for v in values)
        emitted.setdefault(page, set()).update(nav["filters"])
    assert {page: sorted(keys) for page, keys in emitted.items()} == {page: sorted(keys) for page, keys in PAGE_URL_KEYS.items()}


def test_navigate_event_summary_history_and_context_line():
    assert navigation.NAV_KEYS[-1] == "filters"
    # the stream copies every key of the target, filters included, whatever the page
    nav = target(page="digital-twin", label="CT-01 in the digital twin", asset="CT_01", asset_id=4, filters={"severityIds": ["1"]})
    payload = {"navigation": {**nav, "extra": "dropped"}, "status": "opened", "note": "n"}
    assert stream.result_items("open_in_app", payload, True) == [("navigate", nav)]
    assert stream.result_items("open_in_app", {"error": "Unknown page"}, False) == []
    assert stream.result_items("other_tool", payload, True) == []
    assert stream.summarise("open_in_app", payload, True, None) == "Opened: CT-01 in the digital twin"

    call = types.Part(function_call=types.FunctionCall(id="n1", name="open_in_app", args={"page": "digital-twin", "asset": "CT_01"}))
    result = types.Part(function_response=types.FunctionResponse(id="n1", name="open_in_app", response=payload))
    norm = stream.StreamNormaliser(model_of=lambda a: "")
    out = norm.feed(Event(author=ROOT, invocation_id="i1", content=types.ModelContent(parts=[call])))
    out += norm.feed(Event(author=ROOT, invocation_id="i1", content=types.UserContent(parts=[result])))
    assert [e for e, _ in out] == ["agent", "tool_call", "tool_result", "navigate"]
    assert out[2][1]["summary"] == "Opened: CT-01 in the digital twin" and out[3][1] == nav

    events = [
        Event(author="user", invocation_id="i1", content=stream.build_user_content("Take me to CT-01", {"view": "walk"})),
        Event(author=ROOT, invocation_id="i1", content=types.ModelContent(parts=[call])),
        Event(author=ROOT, invocation_id="i1", content=types.UserContent(parts=[result])),
        Event(author=ROOT, invocation_id="i1", content=types.ModelContent(parts=[types.Part(text="CT-01 is now shown.")])),
        Event(author="user", invocation_id="i2", content=types.UserContent(parts=[types.Part(text="Thanks")])),
        Event(author=ROOT, invocation_id="i2", content=types.ModelContent(parts=[types.Part(text="You're welcome.")])),
    ]
    msgs = stream.history_messages(events)
    assert [(m["role"], m["navigations"]) for m in msgs] == [("user", []), ("assistant", [nav]), ("user", []), ("assistant", [])]
    assert msgs[0]["text"] == "Take me to CT-01"

    assert stream.format_context({"page": "digital-twin", "asset_alias": "CT_01", "view": "walk"}) == "[UI context] page=digital-twin; selected asset=CT_01; view=walk"
    assert stream.format_context({"view": "walk<script>alert(1)"}) == "[UI context] view=walkscript"


async def test_root_handles_navigation_itself_and_every_chat_agent_has_the_tool(fake_backend, cleanup_usage):
    svc, models = make_service(
        {
            ROOT: [Call("open_in_app", {"page": "digital-twin", "asset": "CT_01", "view": "walk"}), "You are now walking to CT-01.", Call("transfer_to_agent", {"agent_name": INSIGHT})],
            INSIGHT: [Call("transfer_to_agent", {"agent_name": TWIN})],
            TWIN: ["The twin agent here."],
        }
    )
    try:
        events = await run_turn(svc, "Walk me to cooling tower 1", context={"page": "digital-twin", "view": "walk"})
        sid = first(events, "session")["session_id"]
        history = await svc.get_history(IDENTITY, sid)
        await run_turn(svc, "And the tower data?", session_id=sid)
    finally:
        await svc.close()
    assert [e for e in names(events) if e not in ("text", "usage")] == ["session", "agent", "tool_call", "tool_result", "navigate", "message", "done"]
    assert first(events, "agent") == {"name": ROOT} and first(events, "tool_call")["agent"] == ROOT
    nav = target(page="digital-twin", label="Walk to CT-01 (first person)", asset="CT_01", asset_id=4, view="walk")
    assert first(events, "navigate") == nav
    assert first(events, "message") == {"text": "You are now walking to CT-01.", "agent": ROOT}
    assert history["messages"][1]["navigations"] == [nav]

    root_req = models[ROOT].requests[0]
    assert "[UI context] page=digital-twin; view=walk; plant time=" in request_text(root_req)
    for agent in (ROOT, INSIGHT, TWIN):
        req = models[agent].requests[0]
        assert "open_in_app" in req.tools_dict, agent
        assert "filters" in req.tools_dict["open_in_app"]._get_declaration().parameters_json_schema["properties"], agent
        assert prompts.NAVIGATION in str(req.config.system_instruction), agent
        # every chat agent is LEVO and declines off-topic requests itself
        assert prompts.IDENTITY in str(req.config.system_instruction), agent
        assert prompts.SCOPE in str(req.config.system_instruction), agent


async def test_a_second_navigation_in_one_answer_is_refused(cleanup_usage):
    svc, _ = make_service(
        {
            ROOT: [
                Call("open_in_app", {"page": "digital-twin", "location": "Chiller Plant Room"}),
                Call("open_in_app", {"page": "simulation", "section": "new"}),
                "The chiller plant room is shown.",
                Call("open_in_app", {"page": "simulation", "section": "new"}),
                "The scenario builder is open.",
            ]
        }
    )
    try:
        events = await run_turn(svc, "Show me the chiller plant room")
        sid = first(events, "session")["session_id"]
        later = await run_turn(svc, "Now a new simulation", session_id=sid)
    finally:
        await svc.close()
    results = [d for e, d in events if e == "tool_result"]
    assert [r["ok"] for r in results] == [True, False] and "already opened (or is opening) a page" in results[1]["summary"]
    assert [d["page"] for e, d in events if e == "navigate"] == ["digital-twin"]
    assert [d["page"] for e, d in later if e == "navigate"] == ["simulation"]  # the next answer may navigate


async def test_one_navigation_per_answer_across_parallel_calls_and_agent_transfers(fake_backend, cleanup_usage):
    svc, _ = make_service(
        {
            ROOT: [
                # two calls in one model response (ADK runs them concurrently): the first one wins
                [Call("open_in_app", {"page": "digital-twin", "asset": "CT_01"}), Call("open_in_app", {"page": "simulation", "section": "new"})],
                Call("transfer_to_agent", {"agent_name": INSIGHT}),
            ],
            # the same answer continues in another agent: no second navigation either
            INSIGHT: [Call("open_in_app", {"page": "ai-insights", "tab": "faults"}), "CT-01 is shown; its faults follow."],
        }
    )
    try:
        events = await run_turn(svc, "Show me CT-01 and its faults")
    finally:
        await svc.close()
    calls = {d["id"]: d for e, d in events if e == "tool_call" and d["name"] == "open_in_app"}
    outcome = sorted(
        (d["agent"], calls[d["id"]]["args"].get("asset") or calls[d["id"]]["args"].get("location") or calls[d["id"]]["args"]["page"], d["ok"])
        for e, d in events
        if e == "tool_result" and d["name"] == "open_in_app"
    )
    assert outcome == sorted([(ROOT, "CT_01", True), (ROOT, "simulation", False), (INSIGHT, "ai-insights", False)])
    refused = [d["summary"] for e, d in events if e == "tool_result" and d["name"] == "open_in_app" and not d["ok"]]
    assert len(refused) == 2 and all("already opened (or is opening) a page" in s for s in refused)
    assert [d["asset"] for e, d in events if e == "navigate"] == ["CT_01"]


async def test_walk_mode_and_replay_reach_the_tool_from_the_ui_context(fake_backend, cleanup_usage):
    twin_call = Call("open_in_app", {"page": "digital-twin", "asset": "CT_01"})  # no view: the model leaves it out
    svc, models = make_service({ROOT: [twin_call, "CT-01 is shown.", twin_call, "CT-01 is shown.", twin_call, "CT-01 is shown, live."]})
    try:
        walking = await run_turn(svc, "Show me CT-01", context={"page": "digital-twin", "view": "walk"})
        sid = first(walking, "session")["session_id"]
        orbit = await run_turn(svc, "Show me CT-01 again", session_id=sid, context={"page": "digital-twin"})
        replay = await run_turn(svc, "Show me CT-01", context={"page": "digital-twin", "replay_job_id": 85})
    finally:
        await svc.close()
    assert first(walking, "navigate") == target(page="digital-twin", label="Walk to CT-01 (first person)", asset="CT_01", asset_id=4, view="walk")
    assert first(orbit, "navigate")["view"] is None  # nothing of the earlier turn survives
    replayed = first(replay, "navigate")
    assert replayed["view"] is None and replayed["label"] == "CT-01 in the digital twin (replay of simulation #85)"
    assert "The twin stays in the replay of simulation #85" in function_response(models[ROOT].requests[-1], "open_in_app")["note"]


async def test_navigate_event_and_history_over_http(client):  # noqa: F811 - fixture
    c, models, _ = client
    models[ROOT].script.extend([Call("open_in_app", {"page": "ai-insights", "tab": "faults", "item_id": 43}), "Fault #43 is open in AI Insights."])
    events = await chat(c, {"message": "Open fault 43", "context": {"page": "digital-twin", "view": "walk"}})
    nav = target(page="ai-insights", label="AI Insights: fault #43", tab="faults", item_id=43)
    assert ("navigate", nav) in events
    sid = events[0][1]["session_id"]
    msgs = (await c.get(f"/sessions/{sid}", headers=HEADERS)).json()["messages"]
    assert msgs[1]["navigations"] == [nav] and msgs[1]["text"] == "Fault #43 is open in AI Insights."
    assert "view=walk" in request_text(models[ROOT].requests[0])
