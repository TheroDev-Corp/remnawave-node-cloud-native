variable "TAG" {
    default = "dev"
}

variable "REGISTRIES" {
    default = ["ghcr.io/therodev-corp/remnawave-node-cloud-native"]
}

variable "VARIANTS" {
    default = {
        plain = {
            integrations = ""
            suffix       = ""
        }
    }
}

target "node" {
    name = variant

    matrix = {
        variant = ["plain"]
    }

    context    = "."
    dockerfile = "docker/Dockerfile"
    platforms  = ["linux/amd64"]

    args = {
        INTEGRATIONS = VARIANTS[variant].integrations
    }

    tags = [
        for registry in REGISTRIES : "${registry}:${TAG}${VARIANTS[variant].suffix}"
    ]
}

group "default" {
    targets = ["plain"]
}
