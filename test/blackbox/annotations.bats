# Note: Intended to be run as "make test-annotations"
#       Makefile target installs & checks all necessary tooling
#       Extra tools that are not covered in Makefile target needs to be added in verify_prerequisites()

load helpers_zot

function verify_prerequisites {
    if [ ! $(command -v curl) ]; then
        echo "you need to install curl as a prerequisite to running the tests" >&3
        return 1
    fi

    if [ ! $(command -v jq) ]; then
        echo "you need to install jq as a prerequisite to running the tests" >&3
        return 1
    fi

    if [ ! $(command -v podman) ]; then
        echo "you need to install podman as a prerequisite to running the tests" >&3
        return 1
    fi

    return 0
}

function setup_file() {
    export COSIGN_PASSWORD=""
    # Verify prerequisites are available
    if ! $(verify_prerequisites); then
        exit 1
    fi
    # Download test data to folder common for the entire suite, not just this file
    skopeo --insecure-policy copy --format=oci docker://ghcr.io/project-zot/golang:1.20 oci:${TEST_DATA_DIR}/golang:1.20
    # Setup zot server
    local zot_root_dir=${BATS_FILE_TMPDIR}/zot
    local zot_config_file=${BATS_FILE_TMPDIR}/zot_config.json
    mkdir -p ${zot_root_dir}
    cat > ${zot_config_file}<<EOF
{
    "distSpecVersion": "1.1.0-dev",
    "storage": {
        "rootDirectory": "${zot_root_dir}"
    },
    "http": {
        "address": "0.0.0.0",
        "port": "8080"
    },
    "log": {
        "level": "debug",
        "output": "${BATS_FILE_TMPDIR}/zot.log"
    },
    "extensions":{
        "search": {
                    "enable": "true"
        },
        "lint": {
                    "enable": "true",
                    "mandatoryAnnotations": ["org.opencontainers.image.licenses", "org.opencontainers.image.vendor"]
        }
    }
}
EOF
    cat > ${BATS_FILE_TMPDIR}/stacker.yaml<<EOF
\${{IMAGE_NAME}}:
  from:
    type: docker
    url: docker://\${{IMAGE_NAME}}:\${{IMAGE_TAG}}
  annotations:
    org.opencontainers.image.title: \${{IMAGE_NAME}}
    org.opencontainers.image.description: \${{DESCRIPTION}}
    org.opencontainers.image.licenses: \${{LICENSES}}
    org.opencontainers.image.vendor: \${{VENDOR}}
EOF
    cat > ${BATS_FILE_TMPDIR}/Dockerfile<<EOF
FROM public.ecr.aws/t0x7q1g8/centos:7
CMD ["/bin/sh", "-c", "echo 'It works!'"]
EOF
    zot_serve ${ZOT_PATH} ${zot_config_file}
    wait_zot_reachable 8080
}

function teardown_file() {
    cat ${BATS_FILE_TMPDIR}/zot.log >&3
    zot_stop_all
    run rm -rf ${HOME}/.config/notation
}

@test "build image with podman and specify annotations" {
    run podman build -f ${BATS_FILE_TMPDIR}/Dockerfile -t 127.0.0.1:8080/annotations:latest . --format oci --annotation org.opencontainers.image.vendor="CentOS" --annotation org.opencontainers.image.licenses="GPLv2"
    [ "$status" -eq 0 ]
    run podman push 127.0.0.1:8080/annotations:latest --tls-verify=false --format=oci
    [ "$status" -eq 0 ]
    run curl -X POST -H "Content-Type: application/json" --data '{ "query": "{ ImageList(repo: \"annotations\") { Results { RepoName Tag Manifests {Digest ConfigDigest Size Layers { Size Digest }} Vendor Licenses }}}"}' http://localhost:8080/v2/_zot/ext/search
    [ "$status" -eq 0 ]

    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].RepoName') = '"annotations"' ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].Vendor') = '"CentOS"' ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].Licenses') = '"GPLv2"' ]
}

@test "build image with stacker and specify annotations" {
    run stacker --oci-dir ${BATS_FILE_TMPDIR}/stackeroci --stacker-dir ${BATS_FILE_TMPDIR}/.stacker --roots-dir ${BATS_FILE_TMPDIR}/roots build -f ${BATS_FILE_TMPDIR}/stacker.yaml --substitute IMAGE_NAME="ghcr.io/project-zot/golang" --substitute IMAGE_TAG="1.20" --substitute DESCRIPTION="mydesc" --substitute VENDOR="CentOs" --substitute LICENSES="GPLv2" --substitute COMMIT= --substitute OS=$OS --substitute ARCH=$ARCH
    [ "$status" -eq 0 ]
    run stacker --oci-dir ${BATS_FILE_TMPDIR}/stackeroci --stacker-dir ${BATS_FILE_TMPDIR}/.stacker --roots-dir ${BATS_FILE_TMPDIR}/roots publish -f ${BATS_FILE_TMPDIR}/stacker.yaml --substitute IMAGE_NAME="ghcr.io/project-zot/golang" --substitute IMAGE_TAG="1.20" --substitute DESCRIPTION="mydesc" --substitute VENDOR="CentOs" --substitute LICENSES="GPLv2" --url docker://127.0.0.1:8080 --tag 1.20 --skip-tls
    [ "$status" -eq 0 ]
    run curl -X POST -H "Content-Type: application/json" --data '{ "query": "{ ImageList(repo: \"ghcr.io/project-zot/golang\") { Results { RepoName Tag Manifests {Digest ConfigDigest Size Layers { Size Digest }} Vendor Licenses Description }}}"}' http://localhost:8080/v2/_zot/ext/search
    [ "$status" -eq 0 ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].RepoName') = '"ghcr.io/project-zot/golang"' ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].Description') = '"mydesc"' ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].Vendor') = '"CentOs"' ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].Licenses') = '"GPLv2"' ]
}

@test "sign/verify with cosign" {
    run curl -X POST -H "Content-Type: application/json" --data '{ "query": "{ ImageList(repo: \"annotations\") { Results { RepoName Tag Manifests {Digest ConfigDigest Size Layers { Size Digest }} Vendor Licenses }}}"}' http://localhost:8080/v2/_zot/ext/search
    [ "$status" -eq 0 ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].RepoName') = '"annotations"' ]
    local digest=$(echo "${lines[-1]}" | jq -r '.data.ImageList.Results[0].Manifests[0].Digest')

    run cosign initialize
    [ "$status" -eq 0 ]
    run cosign generate-key-pair --output-key-prefix "${BATS_FILE_TMPDIR}/cosign-sign-test"
    [ "$status" -eq 0 ]
    run cosign sign --key ${BATS_FILE_TMPDIR}/cosign-sign-test.key localhost:8080/annotations:latest --yes
    [ "$status" -eq 0 ]
    run cosign verify --key ${BATS_FILE_TMPDIR}/cosign-sign-test.pub localhost:8080/annotations:latest
    [ "$status" -eq 0 ]
    local sigName=$(echo "${lines[-1]}" | jq '.[].critical.image."docker-manifest-digest"')
    [[ "$sigName" == *"${digest}"* ]]
}

@test "sign/verify with notation" {
    run curl -X POST -H "Content-Type: application/json" --data '{ "query": "{ ImageList(repo: \"annotations\") { Results { RepoName Tag Manifests {Digest ConfigDigest Size Layers { Size Digest }} Vendor Licenses }}}"}' http://localhost:8080/v2/_zot/ext/search
    [ "$status" -eq 0 ]
    [ $(echo "${lines[-1]}" | jq '.data.ImageList.Results[0].RepoName') = '"annotations"' ]
    [ "$status" -eq 0 ]

    run notation cert generate-test "notation-sign-test"
    [ "$status" -eq 0 ]

    local trust_policy_file=${HOME}/.config/notation/trustpolicy.json

    cat >${trust_policy_file} <<EOF
{
    "version": "1.0",
    "trustPolicies": [
        {
            "name": "notation-sign-test",
            "registryScopes": [ "*" ],
            "signatureVerification": {
                "level" : "strict"
            },
            "trustStores": [ "ca:notation-sign-test" ],
            "trustedIdentities": [
                "*"
            ]
        }
    ]
}
EOF

    run notation sign --key "notation-sign-test" --plain-http localhost:8080/annotations:latest
    [ "$status" -eq 0 ]
    run notation verify --plain-http localhost:8080/annotations:latest
    [ "$status" -eq 0 ]
    run notation list --plain-http localhost:8080/annotations:latest
    [ "$status" -eq 0 ]
}
