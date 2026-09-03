"""open_in_app (contract I-1): validation as the user, the `navigate` SSE event, history and walk-mode context."""

from __future__ import annotations

import asyncio
from collections.abc import Iterator
from types import SimpleNamespace
from typing import Any

import pytest
from google.adk.events import Event
from google.genai import types

from afdd_ai.agent import backend_api, navigation, prompts, stream
from afdd_ai.agent.agents import INSIGHT, ROOT, TWIN

from .support import ASSETS, TOKEN, Call, FakeBackend, request_text
from .test_agent_flow import IDENTITY, first, function_response, make_service, names, run_turn
from .test_app_sessions import HEADERS, chat, client, scratch_db  # noqa: F401 - fixtures used by the HTTP test

PERMS = ["aiassistant.read", "digitaltwin.read", "aiinsights.read", "simulation.read", "equipmentoverview.read", "alarmsreport.read"]
# A user whose location scope is FAB1 only (like the demo's fab facilities groups).
FAB1_TOKEN = "tok-fab1-nav"
FAB1_LOCATIONS = navigation.LOCATIONS[:7]


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
        ({"page": "digital-twin", "location": "MAU Plant Room North", "view": "walk"}, "MAU Plant Room North has no 3D model, so walk mode cannot go there"),
        ({"page": "digital-twin", "view": "fly"}, "Unknown view 'fly'. Use one of: walk, orbit."),
        ({"page": "digital-twin", "panel": "alarms"}, "Unknown panel 'alarms'"),
        ({"page": "ai-insights", "tab": "trends"}, "Unknown tab 'trends'"),
        ({"page": "ai-insights", "item_id": 5}, "item_id needs its tab"),
        ({"page": "ai-insights", "tab": "faults", "item_id": "abc"}, "item_id must be a whole number"),
        ({"page": "ai-insights", "tab": "faults", "item_id": -3}, "item_id must be a positive id"),
        ({"page": "simulation", "section": "results"}, "Unknown section 'results'. Use one of: new."),
        ({"page": "asset-overview", "asset": "CH_01", "section": "alarms"}, "Unknown section 'alarms'"),
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

    unit = await open_(page="asset-overview", asset="ct-1", section="SPC", parameter="chwst")
    assert unit["navigation"] == target(page="asset-overview", label="CT-01 equipment details (SPC)", asset="CT_01", asset_id=4, section="spc")
    assert not fake_backend.calls_to("digital_twin/getassetparameters")  # parameter does not apply: not resolved
    listing = await open_(page="asset-overview", section="status")
    assert listing["navigation"] == target(page="asset-overview", label="Equipment overview") and "section" in listing["note"]
    report = await open_(page="alarm-report", asset="CH_01", view="walk")
    assert report["navigation"] == target(page="alarm-report", label="Alarm report")
    assert "Ignored (not used by alarm-report): asset, view." in report["note"]


async def test_units_and_places_without_3d_models_are_flagged(fake_backend):
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
        assert fab["navigation"]["location"] == "Sub-Fab Bay A (Etch)" and "has no 3D model yet" in fab["note"]
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
    empty = await open_(ctx(walk=True), page="digital-twin", location="Sub-Fab Bay A (Etch)")
    assert empty["navigation"]["view"] is None and "nothing in 3D to walk to" in empty["note"]
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
    assert bay["navigation"]["location"] == "Sub-Fab Bay A (Etch)"
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


def test_navigate_event_summary_history_and_context_line():
    nav = target(page="digital-twin", label="CT-01 in the digital twin", asset="CT_01", asset_id=4)
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
        assert prompts.NAVIGATION in str(req.config.system_instruction), agent


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
