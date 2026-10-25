#!/usr/bin/env python3
"""用Go pclntab离线补全macOS sample里的NATS地址；不附加或修改目标进程。"""
import argparse
import re
import subprocess
from pathlib import Path

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('sample',type=Path)
parser.add_argument('--binary',type=Path,required=True)
parser.add_argument('--output',type=Path,required=True)
a=parser.parse_args()
text=a.sample.read_text()
load=int(re.search(r'Load Address:\s+(0x[\da-fA-F]+)',text).group(1),16)
headers=subprocess.check_output(['otool','-l',str(a.binary)],text=True)
base=int(re.search(r'segname __TEXT\s+vmaddr (0x[\da-fA-F]+)',headers).group(1),16)
pattern=r'\?\?\?\s+\(in nats-server\)[^\n]*?\[(0x[\da-fA-F]+)\]'
addresses=sorted(set(re.findall(pattern,text)))
queries=''.join(f'{int(x,16)-load+base:x}\n' for x in addresses)
resolved=subprocess.check_output(['go','tool','addr2line',str(a.binary)],input=queries,text=True).splitlines()
if len(resolved)!=2*len(addresses):raise ValueError('addr2line output mismatch')
names={addr:resolved[2*i]+' ['+resolved[2*i+1]+']' for i,addr in enumerate(addresses)}
a.output.write_text(re.sub(pattern,lambda m:names[m.group(1)]+' (in nats-server) ['+m.group(1)+']',text))
print(f'{len(addresses)} addresses resolved')
