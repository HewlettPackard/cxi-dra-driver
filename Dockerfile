FROM scratch
COPY bin/cxi-dra-driver /cxi-dra-driver
COPY bin/cxi-cdi-hook /cxi-cdi-hook
COPY bin/slingshot-cni /slingshot-cni
COPY bin/ldconfig /ldconfig
COPY bin/libcxi.so.1 /usr/lib64/libcxi.so.1
COPY bin/libnl-3.so.200 /usr/lib64/libnl-3.so.200
COPY bin/libresolv.so.2 /lib64/libresolv.so.2
COPY bin/libc.so.6 /lib64/libc.so.6
COPY bin/ld-linux-aarch64.so.1 /lib/ld-linux-aarch64.so.1
ENTRYPOINT ["/lib/ld-linux-aarch64.so.1", "--library-path", "/usr/lib64:/lib64", "/cxi-dra-driver"]
