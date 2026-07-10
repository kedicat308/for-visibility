#!/usr/bin/env python3
# Minimal BMP / FPM collector: accept the connection FRR dials out to,
# stream-parse message framing, print type + count. Proves the push works.
import socket, struct, sys, time, threading

name, port, mode = sys.argv[1], int(sys.argv[2]), sys.argv[3]
BMP_TYPES = {0:"RouteMonitoring",1:"StatsReport",2:"PeerDown",3:"PeerUp",
             4:"Initiation",5:"Termination",6:"RouteMirroring"}
RTM = {24:"RTM_NEWROUTE",25:"RTM_DELROUTE",28:"RTM_NEWNEXTHOP"}
counts = {}

def log(m): print(f"[{time.strftime('%H:%M:%S')}] {name}: {m}", flush=True)
def bump(k): counts[k] = counts.get(k,0)+1

def handle_bmp(conn):
    buf=b""
    while True:
        d=conn.recv(65535)
        if not d: log(f"peer closed. totals={counts}"); return
        buf+=d
        while len(buf)>=6:
            ver=buf[0]; length=struct.unpack(">I",buf[1:5])[0]
            if ver!=3 or length<6 or length>0xffff:
                buf=buf[1:]; continue          # resync
            if len(buf)<length: break
            msg,buf=buf[:length],buf[length:]
            t=msg[5]; nm=BMP_TYPES.get(t,f"?{t}"); bump(nm)
            log(f"BMP {nm} (len={length})  [{nm} x{counts[nm]}]")

def handle_fpm(conn):
    buf=b""
    while True:
        d=conn.recv(65535)
        if not d: log(f"peer closed. totals={counts}"); return
        buf+=d
        while len(buf)>=4:
            ver,typ=buf[0],buf[1]; length=struct.unpack(">H",buf[2:4])[0]
            if length<4 or len(buf)<length: break
            msg,buf=buf[:length],buf[length:]
            info=""
            if typ==1 and length>=12:          # 1 = netlink payload
                nlt=struct.unpack("<H",msg[8:10])[0]
                info=RTM.get(nlt,f"nl_type={nlt}")
            bump(info or f"type{typ}")
            log(f"FPM ver={ver} type={typ} len={length} {info}  [{info} x{counts.get(info,0)}]")

srv=socket.socket(socket.AF_INET,socket.SOCK_STREAM)
srv.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1)
srv.bind(("0.0.0.0",port)); srv.listen(5)
log(f"listening on :{port} mode={mode}")
h=handle_bmp if mode=="bmp" else handle_fpm
while True:
    conn,addr=srv.accept(); log(f"*** CONNECT from {addr} ***")
    threading.Thread(target=h,args=(conn,),daemon=True).start()
