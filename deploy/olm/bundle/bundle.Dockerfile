FROM scratch

# Core bundle labels. These MUST match deploy/olm/bundle/metadata/annotations.yaml.
LABEL operators.operatorframework.io.bundle.mediatype.v1=registry+v1
LABEL operators.operatorframework.io.bundle.manifests.v1=manifests/
LABEL operators.operatorframework.io.bundle.metadata.v1=metadata/
LABEL operators.operatorframework.io.bundle.package.v1=mitos
LABEL operators.operatorframework.io.bundle.channels.v1=alpha
LABEL operators.operatorframework.io.bundle.channel.default.v1=alpha

# Recommended labels.
LABEL operators.operatorframework.io.metrics.builder=operator-sdk-v1.34.1
LABEL operators.operatorframework.io.metrics.mediatype.v1=metrics+v1
LABEL operators.operatorframework.io.metrics.project_layout=unknown

# Copy the bundle files into the image.
COPY manifests /manifests/
COPY metadata /metadata/
