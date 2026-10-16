// SPDX-FileCopyrightText: SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package tools

// Background processes run as Python scripts in the sandbox's main
// container, over ContainerService.Exec: FeOS has no process API, and the
// guest's toolbox has neither Python nor bash, so the image brings both.
//
// A process is a directory under processRoot: cmd, name, started_at,
// timeout, pgid, start, log and, once it is over, exit. Nothing holds the
// process: it starts in a session of its own, so that the Exec stream that
// created it can end (and its process group be killed) without taking it
// along. The session id doubles as the process group, which is what stop
// signals. The records live in the container's writable layer on the pod's
// disk: they survive a suspend, a hibernation and a cold restore, while the
// processes themselves survive only the first two, and the start-time check
// tells the rest apart.
//
// The scripts take their arguments from argv: python puts "-c" in argv[0],
// so the arguments begin at argv[1], without the placeholder bash -c needs.

// processRoot is where the process records live in the container.
const processRoot = "/var/lib/sandbox-processes"

// processStartScript detaches the process into its own session and prints
// its handle as one JSON line.
const processStartScript = `import json,os,shlex,subprocess,sys,time,uuid
cmd,timeout,name,cwd = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
root = "` + processRoot + `"
tid = time.strftime("%H%M%S") + "-" + uuid.uuid4().hex[:6]
d = os.path.join(root, tid)
os.makedirs(d, exist_ok=True)
# no exec: the wrapper has to outlive the command to record its exit code
inner = cmd if timeout <= 0 else "timeout --signal=TERM --kill-after=10 %d bash -c %s" % (timeout, shlex.quote(cmd))
log = open(os.path.join(d, "log"), "wb")
proc = subprocess.Popen(["bash","-c","%s; echo $? > %s" % (inner, shlex.quote(os.path.join(d,"exit")))],
    cwd=cwd or None, stdout=log, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
    start_new_session=True)
def starttime(pid):
    try:
        # /proc/<pid>/stat after "(comm) ": field 22 overall is starttime
        return open("/proc/%d/stat" % pid).read().rsplit(") ",1)[1].split()[19]
    except OSError:
        return ""
for k,v in (("cmd",cmd),("name",name),("timeout",str(timeout)),
            ("pgid",str(proc.pid)),("start",starttime(proc.pid)),
            ("started_at",str(int(time.time())))):
    open(os.path.join(d,k),"w").write(v)
print(json.dumps({"id":tid,"pgid":proc.pid,"log":os.path.join(d,"log"),
                  "timeout":timeout,"name":name}))
`

// processListScript prints the processes as a JSON array, or "no
// background processes".
const processListScript = `import json,os,time
root = "` + processRoot + `"
def read(d,k,default=""):
    try: return open(os.path.join(d,k)).read().strip()
    except OSError: return default
def procstat(pid):
    try:
        rest = open("/proc/%d/stat" % pid).read().rsplit(") ",1)[1].split()
    except OSError:
        return None
    return {"state": rest[0], "pgrp": rest[2], "start": rest[19]}
def alive(pgid, want_start=""):
    if not pgid: return False
    st = procstat(pgid)
    if not st or st["state"] == "Z":      # a zombie has already ended
        return False
    if st["pgrp"] != str(pgid):           # this pid is somebody else's now
        return False
    return not want_start or st["start"] == want_start
out=[]
for tid in sorted(os.listdir(root)) if os.path.isdir(root) else []:
    d=os.path.join(root,tid)
    pgid=int(read(d,"pgid","0") or 0); timeout=int(read(d,"timeout","0") or 0)
    started=int(read(d,"started_at","0") or 0); exit_code=read(d,"exit")
    running = exit_code == "" and alive(pgid, read(d,"start"))
    row={"id":tid,"name":read(d,"name"),"command":read(d,"cmd")[:200],
         "running":bool(running),"started_at":started,"timeout":timeout}
    if exit_code != "": row["exit_code"]=int(exit_code)
    if running and timeout>0: row["seconds_left"]=max(0, started+timeout-int(time.time()))
    out.append(row)
print(json.dumps(out, indent=2) if out else "no background processes")
`

// processLogsScript prints the last lines of a process's log.
const processLogsScript = `import os,sys
root = "` + processRoot + `"
tid,lines = sys.argv[1], int(sys.argv[2])
path=os.path.join(root,tid,"log")
if not os.path.exists(path):
    print("no such process: %s" % tid); raise SystemExit(1)
with open(path,errors="replace") as f: tail=f.readlines()[-lines:]
sys.stdout.write("".join(tail) or "(no output yet)\n")
`

// processStopScript sends SIGTERM to a process's group, and SIGKILL when
// it is still there two seconds later.
const processStopScript = `import os,signal,sys,time
root = "` + processRoot + `"
tid=sys.argv[1]; d=os.path.join(root,tid)
try:
    pgid=int(open(os.path.join(d,"pgid")).read().strip())
except OSError:
    print("no such process: %s" % tid); raise SystemExit(1)
try:
    want_start=open(os.path.join(d,"start")).read().strip()
except OSError:
    want_start=""
def procstat(pid):
    try:
        rest = open("/proc/%d/stat" % pid).read().rsplit(") ",1)[1].split()
    except OSError:
        return None
    return {"state": rest[0], "pgrp": rest[2], "start": rest[19]}
def alive_pg(pgid, want_start=""):
    if not pgid: return False
    st = procstat(pgid)
    if not st or st["state"] == "Z":      # a zombie has already ended
        return False
    if st["pgrp"] != str(pgid):           # this pid is somebody else's now
        return False
    return not want_start or st["start"] == want_start
def alive():
    return alive_pg(pgid, want_start)
if not alive():
    print("process %s was not running any more" % tid); raise SystemExit(0)
os.killpg(pgid, signal.SIGTERM)
for _ in range(20):
    time.sleep(0.1)
    if not alive(): break
if alive():
    os.killpg(pgid, signal.SIGKILL)
    print("process %s killed" % tid)
else:
    print("process %s stopped" % tid)
`
