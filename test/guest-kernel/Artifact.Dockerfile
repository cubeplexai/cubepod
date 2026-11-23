FROM scratch
LABEL org.opencontainers.image.title="Experimental Kata Firecracker NFSv3 guest kernel"
LABEL io.cubepod.validation="kernel-boot-only-not-volume-validated"
COPY vmlinux-6.18.62-cubepod-nfs1 /kernel/vmlinux
COPY resolved.config /kernel/config
COPY manifest.json /kernel/manifest.json
