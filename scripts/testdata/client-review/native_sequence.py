"""Diagnostic scheduling probe. Pass the reviewed repository root as argv[1]."""
import pathlib, tempfile, subprocess, socket, struct, threading, sys
root=pathlib.Path(sys.argv[1])
source=(root/'client/cpp/src/client.cpp').read_text()
needle='            heartbeat_seq = sequence.fetch_add(1) + 1;'
assert source.count(needle)==1
# Only insert a scheduling hook at the unlocked empty-queue -> heartbeat gap.
source=source.replace(needle,'            review_admit_between_empty_check_and_heartbeat(this);\n'+needle)
source=source.replace('struct roost_client {','void review_admit_between_empty_check_and_heartbeat(roost_client *);\nstruct roost_client {',1)
driver=r'''
void review_admit_between_empty_check_and_heartbeat(roost_client *c) {
 static bool once=false;
 if(once)return; once=true;
 // Model a concurrent application request completing admission in this gap.
 std::thread application([c]{if(!roost_request(c,42,43,nullptr,0))std::abort();});
 application.join();
}
int main(int argc,char **argv){
 auto o=roost_default_options();o.heartbeat_ms=20;
 auto *c=roost_create(o);
 const uint8_t ticket[]={'t'};
 if(!roost_connect(c,"127.0.0.1",uint16_t(std::stoi(argv[1])),ticket,1))return 2;
 auto end=Clock::now()+std::chrono::seconds(3);
 bool done=false;
 while(Clock::now()<end&&!done){roost_event e{};while(roost_poll(c,&e)){if(e.type==ROOST_RESPONSE)done=true;}std::this_thread::sleep_for(std::chrono::milliseconds(1));}
 roost_destroy(c);return done?0:3;
}
'''
def exact(c,n):
 b=b''
 while len(b)<n:
  p=c.recv(n-len(b))
  if not p:raise EOFError()
  b+=p
 return b
with tempfile.TemporaryDirectory(prefix='review-native-seq-') as directory:
 d=pathlib.Path(directory);(d/'probe.cpp').write_text(source+driver)
 subprocess.run(['g++','-std=c++17','-pthread','-I'+str(root/'client/cpp/include'),str(d/'probe.cpp'),'-o',str(d/'probe')],check=True)
 listener=socket.socket();listener.bind(('127.0.0.1',0));listener.listen();listener.settimeout(5)
 proc=subprocess.Popen([str(d/'probe'),str(listener.getsockname()[1])])
 observed=[]
 try:
  with listener.accept()[0] as c:
   c.settimeout(5)
   for _ in range(3):
    magic,version,flags,mid,seq,size=struct.unpack('!2sBBIII',exact(c,16));exact(c,size)
    observed.append((mid,seq))
    c.sendall(struct.pack('!2sBBIII',b'RS',2,0,43 if mid==42 else 0,seq,0))
   assert proc.wait(timeout=5)==0
 finally:
  listener.close()
  if proc.poll() is None:proc.kill();proc.wait()
 print('observed (message_id, sequence):',observed,flush=True)
 assert all(a[1]<b[1] for a,b in zip(observed,observed[1:])), 'native client sends a lower sequence after heartbeat; gateway rejects it'
