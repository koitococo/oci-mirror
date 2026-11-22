variable "OCI_MIRROR_IMAGE" {
  default = "oci-mirror:local"
}

group "default" {
  targets = ["mirror"]
}

target "mirror" {
  context    = "."
  dockerfile = "Dockerfile"
  tags       = [OCI_MIRROR_IMAGE]
}
