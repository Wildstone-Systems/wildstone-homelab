"""Real Radicale 3.8.0 WSGI checks; no network or household credentials."""
import base64
import io
import json
import os
import sys
from pathlib import Path
from urllib.parse import unquote
from radicale import config
from radicale.app import Application

root = Path(sys.argv[1])
sys.path.insert(0, str(root))
registry = json.loads((root / "calendars.json").read_text())
(root / "users").write_text("sam:test-sam\nalex:test-alex\nradicale:test-service\n")
(root / "groups").write_text("calendar_users: sam alex radicale\nadults: alex\n")
configuration = config.load([])
configuration.update({
    "auth": {"type": "htpasswd", "htpasswd_filename": str(root / "users"),
             "htpasswd_encryption": "plain", "delay": "0", "ldap_reader_dn": "uid=radicale,ou=people"},
    "group": {"type": "htgroup", "htgroup_filename": str(root / "groups")},
    "rights": {"type": "from_file", "file": str(root / "rights")},
    "storage": {"type": "wildstone_calendars", "filesystem_folder": str(root / "data"),
                "predefined_collections": '{"personal":{"tag":"VCALENDAR"}}'},
}, "fixture")
app = Application(configuration)

def request(user, method, path, data="", headers=None):
    passwords = {"sam": "test-sam", "alex": "test-alex", "radicale": "test-service"}
    environ = {"REQUEST_METHOD": method, "PATH_INFO": path, "SCRIPT_NAME": "",
               "QUERY_STRING": "", "SERVER_NAME": "test", "SERVER_PORT": "80",
               "SERVER_PROTOCOL": "HTTP/1.1", "wsgi.version": (1, 0),
               "wsgi.url_scheme": "http", "wsgi.input": io.BytesIO(data.encode()),
               "wsgi.errors": sys.stderr, "wsgi.multithread": False,
               "wsgi.multiprocess": False, "wsgi.run_once": False,
               "CONTENT_LENGTH": str(len(data.encode())),
               "HTTP_AUTHORIZATION": "Basic " + base64.b64encode((user+":"+passwords[user]).encode()).decode()}
    environ.update(headers or {})
    statuses = []
    body = b"".join(app(environ, lambda status, headers: statuses.append(status))).decode()
    return int(statuses[0].split()[0]), body

school = "/GROUPS/" + base64.b64encode(b"wildstone_calendar_school").decode() + "/"
private = "/GROUPS/" + base64.b64encode(b"wildstone_calendar_private").decode() + "/"
household = "/GROUPS/Y2FsZW5kYXJfdXNlcnM=/"
for user in ("sam", "alex"):
    status, body = request(user, "PROPFIND", "/" + user + "/", headers={"HTTP_DEPTH": "1"})
    assert status == 207, (status, body)
    body = unquote(body)
    assert school in body and household in body and "personal" in body, body
    assert (private in body) == (user == "alex"), body
    assert "#f59e0bff" in body and "School" in body, body
ics = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nBEGIN:VEVENT\r\nUID:fixture\r\nDTSTART:20260923T100000Z\r\nDTEND:20260923T110000Z\r\nSUMMARY:Fixture\r\nEND:VEVENT\r\nEND:VCALENDAR\r\n"
status, body = request("sam", "PUT", school + "event.ics", ics, {"CONTENT_TYPE": "text/calendar"})
assert status == 403, (status, body)
status, body = request("alex", "PUT", school + "event.ics", ics, {"CONTENT_TYPE": "text/calendar"})
assert status == 201, (status, body)
assert request("sam", "GET", school + "event.ics")[0] == 200
assert request("sam", "DELETE", school + "event.ics")[0] == 403
assert request("sam", "PROPFIND", private, headers={"HTTP_DEPTH": "1"})[0] == 403
assert request("sam", "PUT", "/sam/personal/private.ics", ics, {"CONTENT_TYPE": "text/calendar"})[0] == 201
assert request("radicale", "GET", "/sam/personal/private.ics")[0] == 403
assert request("sam", "MOVE", household + "event.ics", headers={"HTTP_DESTINATION": "http://test" + school + "event.ics", "HTTP_OVERWRITE": "F"})[0] in (403, 404)
assert request("alex", "MOVE", school + "event.ics", headers={"HTTP_DESTINATION": "http://test" + household + "event.ics", "HTTP_OVERWRITE": "F"})[0] == 201
assert request("alex", "GET", household + "event.ics")[0] == 200
assert request("sam", "MOVE", household + "event.ics", headers={"HTTP_DESTINATION": "http://test" + school + "event.ics", "HTTP_OVERWRITE": "F"})[0] == 403
assert request("sam", "GET", household + "event.ics")[0] == 200
assert request("alex", "PUT", school + "event.ics", ics.replace("UID:fixture", "UID:another"), {"CONTENT_TYPE": "text/calendar"})[0] == 201
assert request("alex", "MOVE", school + "event.ics", headers={"HTTP_DESTINATION": "http://test" + household + "event.ics", "HTTP_OVERWRITE": "F"})[0] == 412
assert request("alex", "GET", school + "event.ics")[0] == 200
# Startup must preserve event data while updating display metadata.
app = Application(configuration)
assert request("sam", "GET", household + "event.ics")[0] == 200
print("Radicale discovery, per-calendar rights, MOVE and personal isolation passed")
