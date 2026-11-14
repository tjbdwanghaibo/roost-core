import json,os,pathlib,signal,subprocess,sys,time
# Bound dependency setup/shutdown to the segment's shared deadline.
deadline=float(sys.argv[1]);record=pathlib.Path(sys.argv[2]);args=sys.argv[3:]
started=time.monotonic();process=None;status=0
def interrupted(signum,frame): raise KeyboardInterrupt()
signal.signal(signal.SIGTERM,interrupted)
try:
    if deadline-started<=10: raise TimeoutError('no time remains for dependency command')
    process=subprocess.Popen(args,start_new_session=True)
    status=process.wait(timeout=deadline-time.monotonic()-10)
except (subprocess.TimeoutExpired,KeyboardInterrupt,TimeoutError) as exc:
    status=130 if isinstance(exc,KeyboardInterrupt) else 124
    if process is not None:
        try: os.killpg(process.pid,signal.SIGTERM)
        except ProcessLookupError: pass
        try: process.wait(timeout=max(0.1,min(10,deadline-time.monotonic())))
        except subprocess.TimeoutExpired:
            try: os.killpg(process.pid,signal.SIGKILL)
            except ProcessLookupError: pass
            process.wait()
except OSError as exc:
    print(str(exc),file=sys.stderr);status=126
record.write_text(json.dumps({'args':args,'exit':status,'seconds':time.monotonic()-started,'pid':None if process is None else process.pid})+'\n')
sys.exit(status if 0<=status<256 else 1)
