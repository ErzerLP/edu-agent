#!/usr/bin/env python3
"""Real old-server → current-server rehearsal; synthetic data, no external providers.

Requires prebuilt binaries, Docker access to the dedicated PostgreSQL container,
and Node for the existing workspace model fixture. All databases are freshly
created with issue37_upgrade_ names and dropped in finally. Private evidence
(including a plaintext pg_dump and keys) stays in the explicitly chosen directory.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import signal
import socket
import subprocess
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]
BASELINE = "bc55267e023bb3b5cb77f9da61d81b44d5c6257d"
SPACE = "00000000-0000-4000-8000-000000000001"
GOAL_TEXT = "理解偶数并使用原资料说明依据（旧库升级验收）"
TITLE = "升级后保存的导师正文标题"


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


def uid():
    return str(uuid.uuid4())


class Rehearsal:
    def __init__(self, args):
        self.args = args
        self.private = args.evidence_dir.resolve()
        # Never reuse a directory containing secrets or a previous rehearsal.
        self.private.mkdir(mode=0o700, parents=False, exist_ok=False)
        self.private.chmod(0o700)
        self.events = []
        self.acceptance_failures = []
        self.expected_metadata = "\n".join(
            f"{int(path.name[:6])}:{hashlib.sha256(path.read_bytes()).hexdigest()}"
            for path in sorted((ROOT / "server/migrations").glob("[0-9]*.sql"))
        )
        self.dbs = []
        self.processes = []
        self.server = None
        self.origin = f"http://127.0.0.1:{args.port}"
        self.model_url = f"http://127.0.0.1:{args.port + 1}/v1"
        self.db = "issue37_upgrade_" + secrets.token_hex(6)
        self.restored = self.db + "_restore"
        self.cli_dir = self.private / "cli"
        self.cli_dir.mkdir(mode=0o700)
        self.token = ""
        self.cookie = ""
        self.csrf = ""
        self.env = {
            "PATH": os.environ["PATH"],
            "DATABASE_URL": f"postgres://postgres:postgres@127.0.0.1:{args.pg_port}/{self.db}?sslmode=disable",
            "LISTEN_ADDR": f"127.0.0.1:{args.port}",
            "PUBLIC_BASE_URL": self.origin,
            "MIGRATE_ON_START": "true",
            "MODEL_BASE_URL": self.model_url,
            "MODEL_NAME": "issue37-local-fixture",
            "MODEL_API_KEY": "issue37-synthetic-fixture-key",
            "MODEL_CONTEXT_WINDOW": "32768",
            "MODEL_ENDPOINT_ALLOWLIST": json.dumps([self.model_url]),
            "WEB_UI_ALLOW_LOOPBACK_HTTP": "true",
            "WEB_OFFLINE_ENABLED": "false",
            "COMPANION_ENABLED": "false",
            "ADMIN_UI_ENABLED": "false",
            "NOCTURNE_ENABLED": "false",
            "NOCTURNE_RECONCILIATION_INTERVAL": "1s",
            "GOCACHE": os.environ.get("GOCACHE", "/tmp/issue37-upgrade-gocache"),
        }

    def record(self, name, **details):
        event = {"check": name, **details}
        self.events.append(event)
        print(json.dumps(event, ensure_ascii=False), flush=True)

    def run(self, cmd, *, env=None, input=None, timeout=90):
        result = subprocess.run(cmd, env=env, input=input, capture_output=True,
                                text=True, timeout=timeout, check=False)
        require(result.returncode == 0,
                f"command failed ({result.returncode}): {cmd[0]} {cmd[1:3]}\n{result.stderr}")
        return result.stdout.strip()

    def sql(self, db, query):
        require(db in ["postgres", self.db, self.restored], "database outside rehearsal")
        return self.run(["docker", "exec", self.args.container, "psql", "-X", "-U",
                         "postgres", "-d", db, "-v", "ON_ERROR_STOP=1", "-Atc", query])

    def metadata(self):
        return self.sql(self.db, "SELECT version::text || ':' || checksum FROM schema_migrations ORDER BY version;")

    def tables(self):
        return self.sql(self.db, "SELECT tablename FROM pg_tables WHERE schemaname='public' ORDER BY tablename;")

    def create_db(self, db):
        require(db.startswith("issue37_upgrade_") and db in [self.db, self.restored], "unsafe database name")
        # CREATE DATABASE fails if occupied; only remember databases we created.
        self.sql("postgres", f'CREATE DATABASE "{db}"')
        self.dbs.append(db)

    def stop_server(self):
        if self.server is not None:
            self.server.terminate()
            try:
                self.server.wait(timeout=15)
            except subprocess.TimeoutExpired:
                self.server.kill()
                self.server.wait(timeout=5)
            self.server = None

    def start(self, binary, *, web=False, env=None):
        self.stop_server()
        effective = {**self.env, "WEB_UI_ENABLED": str(web).lower(), **(env or {})}
        log_path = self.private / f"server-{len(self.processes)}.log"
        with log_path.open("w") as log:
            self.server = subprocess.Popen([str(binary), "serve"], env=effective,
                                           stdout=log, stderr=log)
        self.processes.append(self.server)
        for _ in range(150):
            require(self.server.poll() is None,
                    f"server exited; inspect private log {log_path}")
            try:
                status, _, _ = self.http("GET", "/readyz", auth=False, expected=None)
                if status == 200:
                    return
            except (urllib.error.URLError, TimeoutError):
                pass
            time.sleep(0.2)
        raise RuntimeError(f"server not ready; inspect private log {log_path}")

    def http(self, method, path, body=None, *, web=False, auth=True, expected=(200, 201, 202, 204), headers=None):
        h = {"Content-Type": "application/json", "X-Learning-Space-ID": SPACE}
        if web:
            h.update({"Cookie": self.cookie, "Origin": self.origin, "X-CSRF-Token": self.csrf})
        elif auth and self.token:
            h["Authorization"] = "Bearer " + self.token
        h.update(headers or {})
        request = urllib.request.Request(self.origin + path, method=method, headers=h,
                                         data=None if body is None else json.dumps(body).encode())
        try:
            response = urllib.request.urlopen(request, timeout=15)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            raw = response.read()
            data = json.loads(raw) if raw and "json" in response.headers.get("Content-Type", "") else raw.decode()
            if expected is not None:
                require(response.status in expected, f"{method} {path}: {response.status} {data}")
            return response.status, data, dict(response.headers)

    def call(self, method, path, body=None, **kw):
        return self.http(method, path, body, **kw)[1]

    def pairing_code(self, binary, profile="user"):
        return self.run([str(binary), "pairing-code", "create", "--profile", profile],
                        env={**self.env, "MIGRATE_ON_START": "false"})

    def cli(self, *args, input=None):
        env = {"PATH": os.environ["PATH"], "XDG_CONFIG_HOME": str(self.cli_dir),
               "NO_COLOR": "1", "TERM": "dumb"}
        output = self.run([str(self.args.old_cli), *args], env=env, input=input)
        with (self.private / "old-cli.log").open("a") as log:
            log.write(f"$ old-cli {' '.join(args)}\n{output}\n")
        return output

    def fixture(self):
        # Reuse the checked-in teaching fixture; the free chat reply is synthetic.
        script = self.private / "model.mjs"
        script.write_text("import { createServer } from 'node:http';\n"
                          f"import {{ workspaceModel }} from {json.dumps((ROOT / 'clients/web/scripts/workspace-model.mjs').as_uri())};\n"
                          "createServer(async (req,res) => { try {\n"
                          " let raw=''; for await (const chunk of req) raw+=chunk;\n"
                          " const p=JSON.parse(raw); const content=p.response_format ? JSON.stringify(workspaceModel(p)) : '偶数可以被 2 整除。这是本地备份恢复验收正文。';\n"
                          " if(p.stream) { res.setHeader('Content-Type','text/event-stream');\n"
                          " res.write('data: '+JSON.stringify({choices:[{index:0,delta:{role:'assistant',content},finish_reason:null}]})+'\\n\\n');\n"
                          " res.write('data: '+JSON.stringify({choices:[{index:0,delta:{},finish_reason:'stop'}],usage:{prompt_tokens:100,completion_tokens:100,total_tokens:200}})+'\\n\\n');\n"
                          " res.end('data: [DONE]\\n\\n'); return; }\n"
                          " res.setHeader('Content-Type','application/json');\n"
                          " res.end(JSON.stringify({id:'issue37-fixture',object:'chat.completion',choices:[{index:0,message:{role:'assistant',content},finish_reason:'stop'}],usage:{prompt_tokens:100,completion_tokens:100,total_tokens:200}}));\n"
                          " } catch(e) {res.statusCode=400;res.end(JSON.stringify({error:String(e)}));}\n"
                          f"}}).listen({self.args.port + 1},'127.0.0.1');\n")
        with (self.private / "model.log").open("w") as log:
            process = subprocess.Popen(["node", str(script)], stdout=log, stderr=log)
        self.processes.append(process)
        for _ in range(50):
            require(process.poll() is None, "model fixture exited")
            try:
                with socket.create_connection(("127.0.0.1", self.args.port + 1), timeout=0.2):
                    return
            except OSError:
                time.sleep(0.1)
        raise RuntimeError("model fixture did not listen")

    def envelope(self, kind, id, version):
        return {"operation_id": uid(), "payload_schema_version": 1,
                "aggregate_type": kind, "aggregate_id": id, "expected_version": version}

    def session(self):
        return self.call("GET", "/v1/tutoring/sessions/" + self.session_id)

    def action(self, action, **extra):
        view = self.session()
        return self.call("POST", f"/v1/tutoring/sessions/{self.session_id}/actions",
                         {**self.envelope("session", self.session_id, view["session"]["aggregate_version"]),
                          "action": action, **extra})

    def seed(self):
        self.cli("pair", "--server", self.origin, "--name", "issue37-old-cli",
                 input=self.pairing_code(self.args.old_server) + "\n")
        credential_files = list(self.cli_dir.rglob("credential.json"))
        require(len(credential_files) == 1, "isolated CLI credential missing")
        credential = json.loads(credential_files[0].read_text())
        self.token, self.old_device = credential["token"], credential["device_id"]
        self.extra_space = json.loads(self.cli("space", "create", "--name", "旧版学习区"))["id"]
        imported = self.call("POST", "/v1/knowledge/imports", {
            "operation_id": uid(), "expected_parent_revision_id": None, "source": "issue37-upgrade",
            "documents": [{"path": "even.md", "markdown": "# 偶数\n\n偶数可以被 2 整除。2 是偶数，3 是奇数。\n"},
                          {"path": "examples.md", "markdown": "# 偶数示例\n\n偶数如 2、4、6，都可以被 2 整除。\n"}]})
        self.revision = self.call("GET", "/v1/knowledge/revisions/head")["revision"]["revision_id"]
        self.goal_id = uid()
        self.goal = self.call("POST", "/v1/learning/goals", {
            **self.envelope("goal", self.goal_id, 0), "text": GOAL_TEXT, "source": "issue37-upgrade"})["result"]
        self.session_id = uid()
        self.call("POST", "/v1/tutoring/sessions", {
            **self.envelope("session", self.session_id, 0), "goal_revision_id": self.goal["goal_revision_id"]})
        self.action("start_diagnostic")
        for kind in ["route", "activity"]:
            view = self.session()
            retrieval = self.call("POST", "/v1/knowledge/retrievals", {
                "query": "偶数", "knowledge_revision_id": self.revision,
                "query_context_schema_version": "query-context-v1", "context": {"session_id": self.session_id}})
            hits = [{"knowledge_revision_id": self.revision, "node_id": h["node_id"],
                     "node_revision_id": h["node_revision_id"], "document_revision_id": h["document_revision_id"],
                     "range": h["section_range"], "slice": h["canonical_slice"], "slice_sha256": h["slice_sha256"]}
                    for h in retrieval["hits"]]
            require(len(hits) > 0, "old retrieval has no source evidence")
            proposal = self.call("POST", "/v1/tutoring/proposals", {
                "request_id": uid(), "proposal_type": kind, "aggregate_type": "session",
                "aggregate_id": self.session_id, "aggregate_version": view["session"]["aggregate_version"],
                "goal_revision_id": self.goal["goal_revision_id"], **view["session"]["focus"],
                "tutoring_state": view["session"]["state"], "knowledge_revision_id": self.revision,
                "node_revision_ids": list(dict.fromkeys(h["node_revision_id"] for h in hits)),
                "input": {"schema_version": "go-cli-context-v1", "work_item": view["work_item"],
                          "retrieval": {"knowledge_revision_id": self.revision, "hits": hits}}})
            self.action("apply_route" if kind == "route" else "issue_activity", proposal_id=proposal["proposal_id"])
        self.action("present_activity")
        self.action("submit_attempt", answer="A", help="none")
        self.action("record_assessment")
        self.original = self.session()
        require(self.original["work_item"]["attempt"]["answer"] == "A", "old answer not saved")
        self.evidence = self.call("GET", "/v1/learning/evidence")
        require(self.evidence["items"], "old formal evidence missing")
        candidate = self.call("POST", "/v1/memory/candidates", {
            "operation_id": uid(), "payload_schema_version": 1, "content": "旧版记忆候选：先解释再练习",
            "reason": "演练显式保存", "category": "interaction_preference", "sensitivity": "non_sensitive",
            "stability": "transient", "valid_until": "2099-01-01T00:00:00Z"})
        self.candidate_id = candidate["candidate"]["candidate"]["candidate_id"]
        self.record("old_seed", status="pass", migrations=len(self.metadata().splitlines()),
                    goal=self.goal_id, session=self.session_id, evidence_items=len(self.evidence["items"]),
                    candidate=self.candidate_id, knowledge_revision=self.revision)

    def verify_old(self, stage):
        require(self.session()["work_item"]["attempt"] == self.original["work_item"]["attempt"], f"{stage}: old answer changed")
        require(self.session()["work_item"]["activity"] == self.original["work_item"]["activity"], f"{stage}: old activity changed")
        require(self.call("GET", f"/v1/learning/goals/{self.goal_id}")["text"] == GOAL_TEXT, f"{stage}: goal changed")
        evidence = self.call("GET", "/v1/learning/evidence")
        require(evidence["items"] == self.evidence["items"], f"{stage}: formal evidence changed")
        require(self.call("GET", f"/v1/knowledge/revisions/{self.revision}/export")["documents"], f"{stage}: old sources unreadable")
        require(self.call("GET", f"/v1/memory/candidates/{self.candidate_id}")["proposed_content"] == "旧版记忆候选：先解释再练习", f"{stage}: candidate unreadable")
        require(self.call("GET", f"/v1/learning-spaces/{self.extra_space}")["name"] == "旧版学习区", f"{stage}: space changed")
        self.record(stage, status="pass", old_goal_answer_activity_evidence_sources_memory="readable")

    def web_pair(self):
        _, data, headers = self.http("POST", "/v1/web/pairings", {
            "code": self.pairing_code(self.args.server, "settings"), "display_name": "issue37-web"}, auth=False,
            headers={"Origin": self.origin})
        self.cookie = headers["Set-Cookie"].split(";", 1)[0]
        self.csrf = data["csrf_token"]

    def new_data(self):
        self.web_pair()
        self.web_goal = uid()
        self.call("POST", "/v1/learning/goals", {
            **self.envelope("goal", self.web_goal, 0), "text": "升级后 Web 新目标", "source": "issue37-web"}, web=True)
        settings = self.call("GET", "/v1/settings", web=True)
        # Persist a real protected settings file through Cookie + CSRF.
        self.call("PUT", "/v1/settings", {"expected_revision": settings["revision"],
                  "target": "mentor", "mentor_uses_teaching": True, "clear_key": False}, web=True)
        self.conversation_id = uid()
        self.call("POST", "/v1/learning/conversations", {"id": self.conversation_id,
                  "saved": True, "title": TITLE})
        view = self.call("GET", f"/v1/learning/conversations/{self.conversation_id}")
        require(view["conversation"]["title"] == TITLE, "new encrypted title unreadable")
        receipt = self.call("POST", f"/v1/learning/conversations/{self.conversation_id}/turns", {
            "operation_id": uid(), "expected_version": view["conversation"]["version"],
            "prompt": "请用一句话解释偶数（恢复验收）", "request_budget": 4, "token_budget": 20000})
        for _ in range(100):
            view = self.call("GET", f"/v1/learning/conversations/{self.conversation_id}")
            if view["items"] and view["items"][0]["status"] in ["succeeded", "failed", "interrupted", "cancelled", "paused_budget", "partial"]:
                break
            time.sleep(0.2)
        require(view["items"] and "本地备份恢复验收正文" in view["items"][0]["output"], f"mentor body not saved: {view}")
        self.conversation = view
        self.record("new_data", status="pass", web_goal=self.web_goal, encrypted_conversation=self.conversation_id,
                    mentor_run=receipt["run_id"])

    def check_new(self, stage):
        require(self.call("GET", f"/v1/learning/goals/{self.web_goal}")["text"] == "升级后 Web 新目标", f"{stage}: new goal changed")
        view = self.call("GET", f"/v1/learning/conversations/{self.conversation_id}")
        require(view["conversation"]["title"] == TITLE and view["items"] == self.conversation["items"], f"{stage}: encrypted body changed")
        self.record(stage, status="pass", new_goal_title_and_turn="readable_and_unchanged")

    def backup(self):
        self.stop_server()  # Quiesce writers; snapshot and configuration describe one state.
        backup = self.private / "backup"
        backup.mkdir(mode=0o700)
        with (backup / "database.dump").open("wb") as output:
            subprocess.run(["docker", "exec", self.args.container, "pg_dump", "-U", "postgres",
                            "-d", self.db, "-Fc", "--no-owner", "--no-acl"], stdout=output, check=True, timeout=90)
        for source in ["mentor.key", "settings.json", "server-env.json"]:
            shutil.copyfile(self.private / source, backup / source)
        modes = {}
        hashes = {}
        for path in backup.iterdir():
            path.chmod(0o600)
            modes[path.name] = oct(path.stat().st_mode & 0o777)
            hashes[path.name] = hashlib.sha256(path.read_bytes()).hexdigest()
        self.record("backup", status="pass", format="pg_dump custom + config + settings + independent key",
                    directory_mode=oct(backup.stat().st_mode & 0o777), file_modes=modes, sha256=hashes)

    def restore(self):
        self.stop_server()
        if self.restored in self.dbs:
            self.sql("postgres", f'DROP DATABASE "{self.restored}" WITH (FORCE)')
            self.dbs.remove(self.restored)
        self.create_db(self.restored)
        with (self.private / "backup/database.dump").open("rb") as dump:
            subprocess.run(["docker", "exec", "-i", self.args.container, "pg_restore", "-U", "postgres",
                            "-d", self.restored, "--exit-on-error", "--no-owner", "--no-acl"], stdin=dump,
                           check=True, timeout=90, capture_output=True)
        self.env = json.loads((self.private / "backup/server-env.json").read_text())
        self.env["DATABASE_URL"] = self.env["DATABASE_URL"].replace(self.db, self.restored)
        self.env["MENTOR_KEY_FILE"] = str(self.private / "backup/mentor.key")
        self.env["LEARNING_SETTINGS_FILE"] = str(self.private / "backup/settings.json")
        self.start(self.args.server, web=True)

    def permission_checks(self):
        self.stop_server()
        for setting, filename in [("MENTOR_KEY_FILE", "mentor.key"),
                                  ("LEARNING_SETTINGS_FILE", "settings.json")]:
            path = self.private / "backup" / filename
            path.chmod(0o644)
            try:
                result = subprocess.run([str(self.args.server), "serve"],
                                        env={**self.env, "WEB_UI_ENABLED": "true", "MIGRATE_ON_START": "false"},
                                        capture_output=True, text=True, timeout=15)
                require(result.returncode != 0, f"unsafe {setting} permissions accepted")
                self.record("permission_rejection", status="pass", file=filename,
                            rejected_mode="0o644", startup_exit=result.returncode)
            finally:
                path.chmod(0o600)
        self.start(self.args.server, web=True)

    def erase(self):
        grant = self.run([str(self.args.server), "privacy-grant", "create", "--device", self.old_device],
                         env={**self.env, "MIGRATE_ON_START": "false"})
        # Read generation from the public candidate response, not a test-only write.
        generation = self.call("GET", f"/v1/memory/candidates/{self.candidate_id}")["read_generation"]["learner_generation"]
        receipt = self.call("POST", "/v1/privacy/erasures", {
            "operation_id": uid(), "payload_schema_version": 1,
            "expected_current_learner_generation": generation, "reason_code": "learner_request",
            "explicit_confirmation": True}, headers={"X-Privacy-Erasure-Grant": grant})
        for _ in range(100):
            receipt = self.call("GET", "/v1/privacy/erasures/" + receipt["erasure_id"])
            if receipt["status"] == "verified":
                break
            time.sleep(0.2)
        require(receipt["status"] == "verified", "privacy erasure did not converge to verified in 20s")
        required_stores = {"identity_metadata", "knowledge_content", "knowledge_index", "knowledge_artifacts",
                           "learning_event_payload", "learning_typed_payload", "tutoring_payload",
                           "inbox_outbox", "projection_generations", "memory_candidate_delivery", "process_cache"}
        succeeded = {step["store"] for step in receipt["steps"] if step["status"] == "succeeded"}
        require(required_stores <= succeeded, "local privacy scrub verification incomplete")
        residuals = self.sql(self.restored, "SELECT "
                             "(SELECT count(*) FROM learning_goal_revisions WHERE goal_text<>'[redacted]')::text || ':' || "
                             "(SELECT count(*) FROM learning_tutor_turns)::text || ':' || "
                             "(SELECT count(*) FROM learning_tutor_conversations WHERE title IS NOT NULL OR NOT deleted)::text;")
        require(residuals == "0:0:0", "old/new goal or encrypted history scrub incomplete")
        for path in [f"/v1/tutoring/sessions/{self.session_id}",
                     f"/v1/knowledge/revisions/{self.revision}/export",
                     f"/v1/learning/conversations/{self.conversation_id}"]:
            status, body, _ = self.http("GET", path, expected=None)
            require(status in [404, 409, 503] and "本地备份恢复验收正文" not in json.dumps(body, ensure_ascii=False),
                    f"erased content accessible: {path} {status}")
        (self.private / "privacy-receipt.json").write_text(json.dumps(receipt, ensure_ascii=False, indent=2) + "\n")
        self.record("privacy_clear", status="pass", receipt_status=receipt["status"],
                    erasure_id=receipt["erasure_id"], residual_goals_turns_titles=residuals,
                    steps={step["store"]: step["status"] for step in receipt["steps"]})

    def execute(self):
        self.record("versions", baseline=BASELINE, current=self.run(["git", "-C", str(ROOT), "rev-parse", "HEAD"]),
                    old_server_sha256=hashlib.sha256(self.args.old_server.read_bytes()).hexdigest(),
                    old_cli_sha256=hashlib.sha256(self.args.old_cli.read_bytes()).hexdigest(),
                    server_sha256=hashlib.sha256(self.args.server.read_bytes()).hexdigest())
        self.create_db(self.db)
        self.fixture()
        self.start(self.args.old_server)
        self.seed()
        old_metadata = self.metadata()
        old_tables = set(self.tables().splitlines())
        self.stop_server()
        (self.private / "mentor.key").write_bytes(secrets.token_bytes(32))
        (self.private / "mentor.key").chmod(0o600)
        self.env.update({"MENTOR_KEY_FILE": str(self.private / "mentor.key"),
                         "LEARNING_SETTINGS_FILE": str(self.private / "settings.json")})
        self.start(self.args.server, web=True)
        metadata = self.metadata()
        (self.private / "old-migrations.txt").write_text(old_metadata + "\n")
        (self.private / "current-migrations.txt").write_text(metadata + "\n")
        require(metadata.splitlines()[:len(old_metadata.splitlines())] == old_metadata.splitlines(), "historical checksum changed")
        require(metadata == self.expected_metadata and old_tables <= set(self.tables().splitlines()), "migration/table mismatch")
        self.record("migration", status="pass", old_count=len(old_metadata.splitlines()),
                    current_count=len(metadata.splitlines()), historical_checksums="unchanged", tables_removed=[])
        self.verify_old("upgrade")
        # /readyz does not wait for the existing asynchronous progress rebuild.
        first_status = None
        for _ in range(100):
            status, body, _ = self.http("GET", "/v1/learning/progress", expected=None)
            if first_status is None:
                first_status = status
            if status == 200:
                break
            require(status == 503 and body["error"]["code"] == "projection_unavailable",
                    f"unexpected progress failure: {status} {body}")
            time.sleep(0.2)
        require(status == 200, "progress rebuild did not finish in 20s")
        (self.private / "progress-response.json").write_text(json.dumps(body, ensure_ascii=False, indent=2) + "\n")
        self.record("progress_upgrade_rebuild", status="pass", initial_http=first_status, final_http=status)
        failed_commands = []
        for command in [("goal", "show", "--id", self.goal_id), ("space", "list"),
                        ("route",), ("evidence",), ("progress",), ("device", "status")]:
            try:
                self.cli(*command)
                self.record("old_cli_read", status="pass", command=" ".join(command))
            except RuntimeError as error:
                failed_commands.append(" ".join(command))
                self.record("old_cli_read", status="fail", command=" ".join(command), error=str(error))
        if failed_commands:
            self.acceptance_failures.append("old_cli_current_server")
        self.record("old_cli_current_server", status="fail" if failed_commands else "pass", failed_commands=failed_commands)
        self.new_data()
        table_list = self.tables()
        self.start(self.args.server, web=False)
        require(self.metadata() == metadata and self.tables() == table_list, "feature-off changed schema")
        self.http("GET", "/v1/web/session", web=True, expected=(401,))
        self.http("GET", "/app/", auth=False, expected=(404,))
        self.check_new("web_off_bearer_read")
        self.cli("goal", "show", "--id", self.web_goal)
        disposable = uid()
        self.call("POST", "/v1/learning/conversations", {"id": disposable, "saved": True, "title": "关闭 Web 时可清除"})
        view = self.call("GET", f"/v1/learning/conversations/{disposable}")
        self.call("DELETE", f"/v1/learning/conversations/{disposable}", {
            "expected_version": view["conversation"]["version"], "confirmed": True})
        self.http("GET", f"/v1/learning/conversations/{disposable}", expected=(404,))
        self.record("web_off_delete", status="pass", bearer_delete=200, historical_checksums_and_tables="unchanged")
        self.start(self.args.server, web=True)
        self.http("GET", "/v1/web/session", web=True)
        self.check_new("web_reenabled")
        self.verify_old("before_backup")
        (self.private / "server-env.json").write_text(json.dumps(self.env))
        (self.private / "server-env.json").chmod(0o600)
        self.backup()
        self.restore()
        require(self.sql(self.restored, "SELECT version::text || ':' || checksum FROM schema_migrations ORDER BY version;") == metadata, "restored migrations changed")
        self.verify_old("restore_old_data")
        self.check_new("restore_decryption")
        self.http("GET", "/v1/web/session", web=True)
        self.record("restore_login", status="pass", pre_backup_web_cookie=200, pre_backup_cli_bearer=200,
                    migrations=len(metadata.splitlines()))
        self.permission_checks()
        wrong_key = self.private / "wrong.key"
        wrong_key.write_bytes(secrets.token_bytes(32))
        wrong_key.chmod(0o600)
        self.start(self.args.server, web=True, env={"MENTOR_KEY_FILE": str(wrong_key)})
        self.http("GET", f"/v1/learning/conversations/{self.conversation_id}", expected=(503,))
        self.start(self.args.server, web=True)
        self.check_new("correct_key_after_wrong_key")
        self.record("wrong_key", status="pass", encrypted_body_read=503)
        # Revoke another pre-backup device, preserving the operator credential.
        web_identity = self.call("GET", "/v1/web/session", web=True)
        device = web_identity["device"]["id"]
        self.call("DELETE", f"/v1/devices/{device}")
        self.http("GET", "/v1/web/session", web=True, expected=(401,))
        self.record("revoke", status="pass", revoked_web_cookie=401)
        self.erase()
        # Intentionally characterize a raw main-DB snapshot: no invented guarantee.
        self.restore()
        self.http("GET", "/v1/web/session", web=True)
        self.verify_old("raw_snapshot_old_data_returns")
        self.check_new("raw_snapshot_new_data_returns")
        self.record("stale_snapshot_barrier", status="fail", revoked_web_cookie=200,
                    erased_old_and_new_content="returns", snapshot_type="raw main PostgreSQL pg_dump",
                    limitation="snapshot includes pre-revocation identity and pre-clear barriers; no external monotonic fence")
        self.acceptance_failures.append("stale_snapshot_barrier")

    def cleanup(self):
        self.stop_server()
        for process in self.processes:
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
        failures = []
        for db in reversed(self.dbs):
            try:
                self.sql("postgres", f'DROP DATABASE "{db}" WITH (FORCE)')
            except Exception as error:
                failures.append(str(error))
        self.record("cleanup", status="fail" if failures else "pass", databases=self.dbs, errors=failures)
        (self.private / "results.json").write_text(json.dumps(self.events, ensure_ascii=False, indent=2) + "\n")
        for path in self.private.rglob("*"):
            if path.is_file():
                path.chmod(0o600)
        require(not failures, "temporary database cleanup failed")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for option in ["old-server", "old-cli", "server", "evidence-dir"]:
        parser.add_argument("--" + option, type=Path, required=True)
    parser.add_argument("--container", default="edu-agent-acc-postgres")
    parser.add_argument("--pg-port", type=int, default=32995)
    parser.add_argument("--port", type=int, default=33137)
    args = parser.parse_args()
    require(33100 <= args.port <= 33198, "service and fixture ports must be in 33100–33199")
    for binary in [args.old_server, args.old_cli, args.server]:
        require(binary.is_absolute() and binary.is_file(), "prebuilt binary must be an absolute existing path")
    # Fail before creating databases if either listener belongs to another task.
    for port in [args.port, args.port + 1]:
        with socket.socket() as probe:
            probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
            probe.bind(("127.0.0.1", port))
    os.umask(0o077)
    rehearsal = Rehearsal(args)
    def interrupted(signum, frame):
        raise KeyboardInterrupt(f"signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    try:
        rehearsal.execute()
        rehearsal.record("execution", status="complete", acceptance_failures=rehearsal.acceptance_failures)
    except Exception as error:
        rehearsal.record("execution", status="error", error=str(error))
        raise
    finally:
        rehearsal.cleanup()


if __name__ == "__main__":
    main()
