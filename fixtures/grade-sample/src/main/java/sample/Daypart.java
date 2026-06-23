package sample;

// Maps an hour (0-23) to a part of the day. Several branches; one condition is wrong.
public class Daypart {
    public String of(int hour) {
        if (hour < 0 || hour > 23) {
            return "invalid";
        }
        if (hour < 6) {
            return "night";
        }
        if (hour > 12) {
            return "morning";
        }
        if (hour < 18) {
            return "afternoon";
        }
        return "evening";
    }
}
