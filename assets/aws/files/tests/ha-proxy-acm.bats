write_confd_file() {
    cat << EOF > ${TELEPORT_CONFD_DIR?}/conf
TELEPORT_ROLE=proxy
EC2_REGION=us-west-2
TELEPORT_AUTH_SERVER_LB=gus-tftestkube4-auth-0f66dd17f8dd9825.elb.us-east-1.amazonaws.com
TELEPORT_CLUSTER_NAME=gus-tftestkube4
TELEPORT_DOMAIN_NAME=gus-tftestkube4.gravitational.io
TELEPORT_INFLUXDB_ADDRESS=http://gus-tftestkube4-monitor-ae7983980c3419ab.elb.us-east-1.amazonaws.com:8086
TELEPORT_PROXY_SERVER_LB=gus-tftestkube4-proxy-bc9ba568645c3d80.elb.us-east-1.amazonaws.com
TELEPORT_PROXY_SERVER_NLB_ALIAS=gus-tftestkube-nlb.gravitational.io
TELEPORT_S3_BUCKET=gus-tftestkube4.gravitational.io
USE_ACM=true
EOF
}

load fixtures/common

@test "[${TEST_SUITE?}] config file was generated without error" {
    [ ${GENERATE_EXIT_CODE?} -eq 0 ]
}

@test "[${TEST_SUITE?}] teleport.auth_servers is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    cat "${TELEPORT_CONFIG_PATH?}"
    cat "${TELEPORT_CONFIG_PATH?}" | grep -E "^  auth_servers:" -A1 | grep -q "${TELEPORT_AUTH_SERVER_LB?}"
}

# in each test, we echo the block so that if the test fails, the block is outputted
@test "[${TEST_SUITE?}] proxy_service.public_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  public_addr:" ${TELEPORT_CONFIG_PATH?} | grep -q "${TELEPORT_DOMAIN_NAME?}:443"
}

@test "[${TEST_SUITE?}] proxy_service.ssh_public_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  ssh_public_addr:" | grep -q "${TELEPORT_PROXY_SERVER_NLB_ALIAS?}:3023"
}

@test "[${TEST_SUITE?}] proxy_service.tunnel_public_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  tunnel_public_addr:" | grep -q "${TELEPORT_PROXY_SERVER_NLB_ALIAS?}:3024"
}

@test "[${TEST_SUITE?}] proxy_service.listen_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  listen_addr: " | grep -q "0.0.0.0:3023"
}

@test "[${TEST_SUITE?}] proxy_service.tunnel_listen_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  tunnel_listen_addr: " | grep -q "0.0.0.0:3024"
}

@test "[${TEST_SUITE?}] proxy_service.web_listen_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  web_listen_addr: " | grep -q "0.0.0.0:3080"
}

@test "[${TEST_SUITE?}] proxy_service.kubernetes.public_addr is set correctly" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  kubernetes:" -A3 | grep -E "^    public_addr" | grep -q "['${TELEPORT_PROXY_SERVER_NLB_ALIAS?}:3026']"
}

@test "[${TEST_SUITE?}] proxy_service.kubernetes support is enabled" {
    load ${TELEPORT_CONFD_DIR?}/conf
    echo "${PROXY_BLOCK?}"
    echo "${PROXY_BLOCK?}" | grep -E "^  kubernetes:" -A3 | grep -q -E "^    enabled: yes"
}
