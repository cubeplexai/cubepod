"""Read/write only this process's anonymous KVM VM, vCPU and RAM."""
import ctypes,fcntl,json,mmap,os,platform,signal,struct
fds=[]; mappings=[]
result={'kernel':platform.release(),'architecture':platform.machine()}
def timed_out(*_):raise TimeoutError('KVM_RUN exceeded 5 seconds')
signal.signal(signal.SIGALRM,timed_out)
try:
 assert platform.machine()=='x86_64','This small real-mode guest is x86_64 only'
 kvm=os.open('/dev/kvm',os.O_RDWR|os.O_CLOEXEC);fds.append(kvm)
 result['api_version']=fcntl.ioctl(kvm,0xAE00,0);assert result['api_version']==12
 vm=fcntl.ioctl(kvm,0xAE01,0);fds.append(vm);result['create_vm']='ok'
 ram=mmap.mmap(-1,1024*1024);mappings.append(ram);ram[0x1000]=0xf4 # actual x86 HLT instruction
 ptr=ctypes.addressof(ctypes.c_char.from_buffer(ram))
 fcntl.ioctl(vm,0x4020AE46,struct.pack('=IIQQQ',0,0,0,len(ram),ptr))
 vcpu=fcntl.ioctl(vm,0xAE41,0);fds.append(vcpu);result['create_vcpu']='ok'
 sregs=bytearray(312);fcntl.ioctl(vcpu,0x8138AE83,sregs,True)
 struct.pack_into('=Q',sregs,0,0);struct.pack_into('=H',sregs,12,0)
 fcntl.ioctl(vcpu,0x4138AE84,sregs)
 regs=[0]*18;regs[16]=0x1000;regs[17]=2
 fcntl.ioctl(vcpu,0x4090AE82,struct.pack('=18Q',*regs))
 run=mmap.mmap(vcpu,fcntl.ioctl(kvm,0xAE04,0),flags=mmap.MAP_SHARED,prot=mmap.PROT_READ|mmap.PROT_WRITE);mappings.append(run)
 signal.alarm(5);fcntl.ioctl(vcpu,0xAE80,0);signal.alarm(0)
 result['guest_exit_reason']=struct.unpack_from('=I',run,8)[0]
 assert result['guest_exit_reason']==5,'Expected actual guest HLT exit'
 result['guest_instruction']='executed HLT via KVM_RUN (not a Linux/FC/CLH guest)'
except Exception as e:
 result['error']=repr(e)
finally:
 signal.alarm(0)
 for mapping in reversed(mappings):mapping.close()
 for fd in reversed(fds):os.close(fd)
print(json.dumps(result,indent=2))
if 'error' in result:raise SystemExit(1)
