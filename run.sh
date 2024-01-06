#!/bin/bash -e
CLUSTER_NAME=""
AWS_REGION="eu-west-1"
IMAGE_PLATFORM="linux/amd64"
POSITIONAL_ARGS=()
export AWS_REGION

number_of_args="$#"
while (( number_of_args > 0 )); do
  case "$1" in
    "--cluster-name")
      CLUSTER_NAME="$2"
      shift
      shift
      ;;
    "--region")
      AWS_REGION="$2"
      export AWS_REGION
      shift
      shift
      ;;
    "--platform")
      IMAGE_PLATFORM="$2"
      shift
      shift
      ;;
    *)
      POSITIONAL_ARGS+=("$1")
      shift
  esac
  number_of_args="$#"
done

set -- "${POSITIONAL_ARGS[@]}" # restore positional parameters

function delete_stack() {
  echo "Deleting stack $1"
  aws cloudformation delete-stack --stack-name "$1"
  while aws cloudformation describe-stacks --stack-name "$1" &> /dev/null; do
    echo "Stack $1 still exists. Waiting"
    sleep 20
  done
  echo "Stack $1 deleted"
}

function get_oidc_id() {
    oidc_id=$(aws eks describe-cluster \
      --name "${CLUSTER_NAME}" \
      --query "cluster.identity.oidc.issuer" \
      --output text | cut -d '/' -f 5)
}

function get_account_id() {
  account_id=$(aws sts get-caller-identity | jq -r '.Account')
}

function get_stack_outputs() {
  outputs=$(aws cloudformation describe-stacks --stack-name "$1" | jq -r '.Stacks[0].Outputs | INDEX(.OutputKey)')
}

function get_ecr_uri() {
  get_stack_outputs "${CLUSTER_NAME}-pomidor-ecr"
  uri=$(jq -r '.RepositoryUri.OutputValue' <<< "$outputs")
}

function get_role_arn() {
  get_stack_outputs "${CLUSTER_NAME}-pomidor-iam"
  role_arn=$(jq -r '.RoleArn.OutputValue' <<< "$outputs")
}

function deploy_image() {
  get_account_id
  get_ecr_uri
  timestamp=$(date +%s)
  image="${uri}:${timestamp}"
  aws ecr get-login-password \
    | docker login --username AWS --password-stdin "${account_id}.dkr.ecr.${AWS_REGION}.amazonaws.com"
  make docker-buildx IMG="${image}" PLATFORMS="${IMAGE_PLATFORM}"
}

function install() {
  if [ "${CLUSTER_NAME}" = "" ]; then
    echo "Cluster Name should be specified"
    exit 255
  fi
  pushd infra
  get_oidc_id
  local namespace="pomidor-system"
  local service_account_name="pomidor-controller-manager"
  aws cloudformation deploy \
    --stack-name "${CLUSTER_NAME}-pomidor-iam" \
    --capabilities "CAPABILITY_IAM" "CAPABILITY_NAMED_IAM" \
    --template-file iam.yaml \
    --parameter-overrides "Namespace=${namespace}" "ServiceAccount=${service_account_name}" \
    "OidcId=${oidc_id}" "ClusterName=${CLUSTER_NAME}"
  aws cloudformation deploy \
    --stack-name "${CLUSTER_NAME}-pomidor-ecr" \
    --template-file ecr.yaml \
    --parameter-overrides "ClusterName=${CLUSTER_NAME}"
  popd

  echo "Deploying image"
  deploy_image
  make manifests
  echo "Installing CRD"
  make install
  get_role_arn
  echo "Deploying manager. ClusterName: ${CLUSTER_NAME}, Image:${image}, RoleArn: ${role_arn}"
  make deploy CLUSTER_NAME="${CLUSTER_NAME}" IMG="${image}" ROLE_ARN="${role_arn}"
}

function uninstall() {
  if [ "${CLUSTER_NAME}" = "" ]; then
    echo "Cluster Name should be specified"
    exit 255
  fi
  make undeploy
  pushd infra
  delete_stack "${CLUSTER_NAME}-pomidor-iam"
  delete_stack "${CLUSTER_NAME}-pomidor-ecr"
  popd
}

case "$1" in
  "install") install ;;
  "uninstall") uninstall ;;
esac