variable "DEST" {
  default = "out/development"
}

group "default" {
  targets = ["core", "layers"]
}

target "core" {
  context    = "."
  dockerfile = "development/Dockerfile"
  platforms  = ["linux/amd64", "linux/arm64"]
  target     = "core"
  output     = ["type=oci,dest=${DEST}/core,tar=false"]
}

target "layers" {
  inherits        = ["core"]
  target          = "layers"
  no-cache-filter = ["seed"]
  output          = ["type=local,dest=${DEST}/layers"]
}
