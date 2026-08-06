"""Generated documents are filed at the deepest location that covers their equipment."""

from afdd_ai.rag.corpus import common_location

# 1 site > 8 CUB building > 9 CUB L1 > {10 chiller plant room, 11 PCW room}; 8 > 13 roof > 14 tower yard
PARENTS = {1: None, 8: 1, 9: 8, 10: 9, 11: 9, 13: 8, 14: 13}


def test_common_location():
    assert common_location([{"location_id": 10}, {"location_id": 10}], PARENTS) == 10
    assert common_location([{"location_id": 10}, {"location_id": 11}], PARENTS) == 9
    assert common_location([{"location_id": 10}, {"location_id": 14}], PARENTS) == 8
    assert common_location([{"location_id": 14}], PARENTS) == 14
    assert common_location([{"location_id": 10}, {"location_id": None}], PARENTS) is None
    assert common_location([], PARENTS) is None
