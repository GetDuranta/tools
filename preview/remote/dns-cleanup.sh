#!/usr/bin/env bash
set -uo pipefail

state_file="${DURANTA_PREVIEW_STATE:-/var/lib/duranta-preview/instance.env}"
if [[ ! -f "$state_file" ]]; then
  exit 0
fi

source "$state_file"
export AWS_DEFAULT_REGION="${AWS_DEFAULT_REGION:-us-west-2}"

marker="$(jq -nc --arg instanceId "$INSTANCE_ID" --arg owner "$OWNER" '{managedBy:"duranta-preview",instanceId:$instanceId,owner:$owner}')"
marker_value="$(jq -Rnr --arg marker "$marker" '$marker | @json')"
records="$(aws route53 list-resource-record-sets --hosted-zone-id "$HOSTED_ZONE_ID")" || exit 1

exact_record() {
  local name="$1"
  local type="$2"
  local value="$3"
  jq -e --arg name "$name" --arg type "$type" --arg value "$value" '
    [.ResourceRecordSets[] | select(
      (.Name | rtrimstr(".")) == $name
      and .Type == $type
      and .TTL == 60
      and (.ResourceRecords | length) == 1
      and .ResourceRecords[0].Value == $value
    )] | length == 1
  ' <<<"$records" >/dev/null
}

marker_name="_duranta-preview.$PREVIEW_HOSTNAME"
if ! exact_record "$PREVIEW_HOSTNAME" A "$PUBLIC_IP" \
  || ! exact_record "*.$PREVIEW_HOSTNAME" A "$PUBLIC_IP" \
  || ! exact_record "$marker_name" TXT "$marker_value"
then
  echo "DNS ownership no longer matches $INSTANCE_ID; refusing cleanup" >&2
  exit 1
fi

batch="$(jq -nc \
  --arg hostname "$PREVIEW_HOSTNAME" \
  --arg wildcard "*.$PREVIEW_HOSTNAME" \
  --arg marker_name "$marker_name" \
  --arg public_ip "$PUBLIC_IP" \
  --arg marker_value "$marker_value" '
  {Changes:[
    {Action:"DELETE",ResourceRecordSet:{Name:$hostname,Type:"A",TTL:60,ResourceRecords:[{Value:$public_ip}]}},
    {Action:"DELETE",ResourceRecordSet:{Name:$wildcard,Type:"A",TTL:60,ResourceRecords:[{Value:$public_ip}]}},
    {Action:"DELETE",ResourceRecordSet:{Name:$marker_name,Type:"TXT",TTL:60,ResourceRecords:[{Value:$marker_value}]}}
  ]}
')"

aws route53 change-resource-record-sets \
  --hosted-zone-id "$HOSTED_ZONE_ID" \
  --change-batch "$batch" >/dev/null
