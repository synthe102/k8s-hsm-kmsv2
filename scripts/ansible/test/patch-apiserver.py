#!/usr/bin/env python3
"""
patch-apiserver.py – Idempotently patch /etc/kubernetes/manifests/kube-apiserver.yaml
to enable envelope encryption via the KMS v2 plugin.

Adds (if not already present):
  - --encryption-provider-config flag
  - enc  volumeMount (read-only, /etc/kubernetes/enc)
  - kms-socket volumeMount (/run/kms-plugin)
  - enc  hostPath volume
  - kms-socket hostPath volume

Prints "kube-apiserver manifest patched" when changes are made, or
"kube-apiserver manifest already up to date" when already idempotent.
Ansible uses the output to determine changed_when.
"""
import re

MANIFEST = "/etc/kubernetes/manifests/kube-apiserver.yaml"

ENC_FLAG = "    - --encryption-provider-config=/etc/kubernetes/enc/encryption-config.yaml"
ENC_MOUNT = "    - mountPath: /etc/kubernetes/enc\n      name: enc\n      readOnly: true"
KMS_MOUNT = "    - mountPath: /run/kms-plugin\n      name: kms-socket"
ENC_VOL = "  - hostPath:\n      path: /etc/kubernetes/enc\n      type: DirectoryOrCreate\n    name: enc"
KMS_VOL = "  - hostPath:\n      path: /run/kms-plugin\n      type: DirectoryOrCreate\n    name: kms-socket"

with open(MANIFEST) as f:
    txt = f.read()

changed = False

# 1. Add --encryption-provider-config flag after --tls-private-key-file
if "--encryption-provider-config" not in txt:
    txt = re.sub(
        r'(    - --tls-private-key-file=[^\n]+)',
        r'\1\n' + ENC_FLAG,
        txt,
    )
    changed = True

# 2. Add enc volumeMount after usr-share-ca-certificates mount
if "name: enc" not in txt:
    txt = re.sub(
        r'(    - mountPath: /usr/share/ca-certificates\n      name: usr-share-ca-certificates\n      readOnly: true)',
        r'\1\n' + ENC_MOUNT,
        txt,
    )
    changed = True

# 3. Add kms-socket volumeMount after enc mount
if "name: kms-socket" not in txt:
    txt = txt.replace(ENC_MOUNT, ENC_MOUNT + "\n" + KMS_MOUNT)
    changed = True

# 4. Add enc hostPath volume after usr-share-ca-certificates volume
if "\n    name: enc\n" not in txt:
    txt = re.sub(
        r'(  - hostPath:\n      path: /usr/share/ca-certificates\n      type: DirectoryOrCreate\n    name: usr-share-ca-certificates)',
        r'\1\n' + ENC_VOL,
        txt,
    )
    changed = True

# 5. Add kms-socket hostPath volume after enc volume
if "\n    name: kms-socket\n" not in txt:
    txt = txt.replace(ENC_VOL, ENC_VOL + "\n" + KMS_VOL)
    changed = True

with open(MANIFEST, "w") as f:
    f.write(txt)

if changed:
    print("kube-apiserver manifest patched")
else:
    print("kube-apiserver manifest already up to date")
